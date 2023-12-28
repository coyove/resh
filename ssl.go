//go:build cgo

package resh

/*
#cgo !arm64crossamd64 LDFLAGS: -lssl -lcrypto -ldl -static-libgcc
#cgo arm64crossamd64 CFLAGS: -I/usr/openssl-amd64/include
#cgo arm64crossamd64 LDFLAGS: -lssl -lcrypto -ldl -static-libgcc -L/usr/openssl-amd64/lib
#include<string.h>
#include<openssl/bio.h>
#include<openssl/ssl.h>
#include<openssl/err.h>

typedef SSL_CTX X_SSL_CTX;
typedef SSL X_SSL;

int X_SSL_ERROR_WANT_WRITE = SSL_ERROR_WANT_WRITE;
int X_SSL_ERROR_WANT_READ = SSL_ERROR_WANT_READ;
int X_SSL_ERROR_ZERO_RETURN = SSL_ERROR_ZERO_RETURN;

char* X_get_last_error() {
    BIO* bio = BIO_new(BIO_s_mem());
    ERR_print_errors(bio);
    char* buf;
    size_t len = BIO_get_mem_data(bio, &buf);
    char* copy = (char *)malloc(len + 1);
    strncpy(copy, buf, len);
    copy[len] = 0;
    BIO_free(bio);
    return copy;
}

X_SSL_CTX* X_init(const char* cert, size_t cert_len, const char* key, size_t key_len) {
    SSL_load_error_strings();
    SSL_library_init();

    SSL_CTX* ctx = SSL_CTX_new(TLS_server_method());
    if (ctx == 0) return NULL;

    BIO* certBio = BIO_new(BIO_s_mem());
    BIO_write(certBio, cert, cert_len);
    X509* certX509 = PEM_read_bio_X509(certBio, NULL, NULL, NULL);
    BIO_free(certBio);

    int r = SSL_CTX_use_certificate(ctx, certX509);
    if (r <= 0) goto FAIL;

    BIO* bo = BIO_new( BIO_s_mem() );
    BIO_write(bo, key, key_len);
    EVP_PKEY* pkey = 0;
    PEM_read_bio_PrivateKey(bo, &pkey, 0, 0);
    BIO_free(bo);

    r = SSL_CTX_use_PrivateKey(ctx, pkey);
    if (r <= 0) goto FAIL;

    r = SSL_CTX_check_private_key(ctx);
    if (r <= 0) goto FAIL;

    return ctx;

FAIL:
    SSL_CTX_free(ctx);
    return NULL;
}

X_SSL* X_accept(SSL_CTX *ctx, int fd) {
    SSL *ssl = SSL_new(ctx);
    if (ssl == NULL) return NULL;

    int r = SSL_set_fd(ssl, fd);
    if (r == 0) {
        SSL_shutdown(ssl);
        SSL_free(ssl);
        return NULL;
    }

    SSL_set_accept_state(ssl);
    return ssl;
}

static int get_error(SSL* ssl, int r) {
    int err = SSL_get_error(ssl, r);
    if (err == SSL_ERROR_SYSCALL) {
        return -(10000 + errno);
    }
    return -err;
}

int X_handshake(SSL* ssl) {
    int r = SSL_do_handshake(ssl);
    if (r == 1) {
        return 1;
    }
    return get_error(ssl, r);
}

int X_read(SSL* ssl, void* buf, size_t len) {
    int rd = SSL_read(ssl, buf, len);
    if (rd > 0) {
        return rd;
    }
    return get_error(ssl, rd);
}

int X_write(SSL* ssl, void* buf, size_t len) {
    int rd = SSL_write(ssl, buf, len);
    if (rd > 0) {
        return rd;
    }
    return get_error(ssl, rd);
}

void X_shutdown(SSL* ssl) {
    SSL_shutdown(ssl);
}

void X_free(SSL* ssl) {
    SSL_free(ssl);
}

void X_SSL_CTX_free(SSL_CTX *ctx) {
    SSL_CTX_free(ctx);
}
*/
import "C"
import (
	"fmt"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

func sslGetError() string {
	str := C.X_get_last_error()
	s := C.GoString(str)
	C.free(unsafe.Pointer(str))
	return strings.TrimSpace(s)
}

func sslGetErrorMaybeErrno(rd int) string {
	if -rd >= 10000 {
		return syscall.Errno(-rd - 10000).Error()
	}
	return strconv.Itoa(rd) + " " + sslGetError()
}

type SSLCtx struct {
	ctx *C.X_SSL_CTX
}

func sslNewCtx(cert, key []byte) (*SSLCtx, error) {
	ccert := C.CString(*(*string)(unsafe.Pointer(&cert)))
	defer C.free(unsafe.Pointer(ccert))

	ckey := C.CString(*(*string)(unsafe.Pointer(&key)))
	defer C.free(unsafe.Pointer(ckey))

	res := C.X_init(ccert, C.ulong(len(cert)), ckey, C.ulong(len(key)))
	if res == nil {
		return nil, fmt.Errorf("failed to init openssl: %v", sslGetError())
	}
	return &SSLCtx{ctx: res}, nil
}

type SSL struct {
	fd         int
	ssl        *C.X_SSL
	handshaked bool
}

func (ctx *SSLCtx) accept(fd int) (*SSL, error) {
	ssl := C.X_accept(ctx.ctx, C.int(fd))
	if ssl == nil {
		return nil, fmt.Errorf("failed to accept: %v", sslGetError())
	}
	return &SSL{
		ssl:        ssl,
		fd:         fd,
		handshaked: false,
	}, nil
}

func (ctx *SSLCtx) close() {
	C.X_SSL_CTX_free(ctx.ctx)
}

func (s *SSL) Read(p []byte) (int, error) {
	if !s.handshaked {
		res := C.X_handshake(s.ssl)
		switch res {
		case 1:
			s.handshaked = true
		case -C.X_SSL_ERROR_WANT_WRITE:
			return -1, fmt.Errorf("failed to handshake, TODO: retry write")
		case -C.X_SSL_ERROR_WANT_READ:
		default:
			return -1, fmt.Errorf("failed to handshake: %v", sslGetErrorMaybeErrno(int(res)))
		}
		return -1, syscall.EAGAIN
	}
	rd := C.X_read(s.ssl, unsafe.Pointer(&p[0]), C.ulong(len(p)))
	if rd > 0 {
		return int(rd), nil
	}
	if rd == -C.X_SSL_ERROR_ZERO_RETURN {
		return 0, nil
	}
	if rd == -C.X_SSL_ERROR_WANT_READ {
		return -1, syscall.EAGAIN
	}
	return -1, fmt.Errorf("failed to read: %v", sslGetErrorMaybeErrno(int(rd)))
}

func (s *SSL) Write(p []byte) (int, error) {
	if !s.handshaked {
		return -1, fmt.Errorf("write before handshaking")
	}
	rd := C.X_write(s.ssl, unsafe.Pointer(&p[0]), C.ulong(len(p)))
	if rd > 0 {
		return int(rd), nil
	}
	if rd == -C.X_SSL_ERROR_WANT_WRITE {
		return int(rd), syscall.EAGAIN
	}
	return -1, fmt.Errorf("failed to write: %v", sslGetErrorMaybeErrno(int(rd)))
}

func (s *SSL) Close() {
	// if s.handshaked {
	// 	C.X_shutdown(s.ssl)
	// }
	C.X_free(s.ssl)
}
