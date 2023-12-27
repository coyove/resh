//go:build cgo

package resh

/*
#cgo LDFLAGS: -lcrypto -lssl
#include<string.h>
#include<openssl/bio.h>
#include<openssl/ssl.h>
#include<openssl/err.h>

typedef SSL_CTX X_SSL_CTX;
typedef SSL X_SSL;

int X_SSL_ERROR_WANT_WRITE = SSL_ERROR_WANT_WRITE;
int X_SSL_ERROR_WANT_READ = SSL_ERROR_WANT_READ;

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

X_SSL_CTX* X_init(const char* cert, const char* key) {
    SSL_load_error_strings();
    SSL_library_init();

    SSL_CTX* ctx = SSL_CTX_new (SSLv23_method ());
    if (ctx == 0) return NULL;

    int r = SSL_CTX_use_certificate_file(ctx, cert, SSL_FILETYPE_PEM);
    if (r <= 0) goto FAIL;

    r = SSL_CTX_use_PrivateKey_file(ctx, key, SSL_FILETYPE_PEM);
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

int X_handshake(SSL* ssl) {
    int r = SSL_do_handshake(ssl);
    if (r == 1) {
        return 0;
    }
    return SSL_get_error(ssl, r);
}

int X_read(SSL* ssl, void* buf, size_t len) {
    int rd = SSL_read(ssl, buf, len);
    if (rd > 0) {
        return rd;
    }
    return SSL_get_error(ssl, rd);
}

int X_write(SSL* ssl, void* buf, size_t len) {
    int rd = SSL_write(ssl, buf, len);
    if (rd > 0) {
        return rd;
    }
    return SSL_get_error(ssl, rd);
}

void X_close(SSL* ssl) {
    SSL_shutdown(ssl);
    SSL_free(ssl);
}
*/
import "C"
import (
	"fmt"
	"syscall"
	"unsafe"

	"github.com/coyove/resh/internal"
)

func sslGetError() string {
	str := C.X_get_last_error()
	s := C.GoString(str)
	C.free(unsafe.Pointer(str))
	return s
}

type SSLCtx struct {
	ctx *C.X_SSL_CTX
}

func sslNewCtx(cert, key string) (*SSLCtx, error) {
	ccert := C.CString(cert)
	defer C.free(unsafe.Pointer(ccert))

	ckey := C.CString(key)
	defer C.free(unsafe.Pointer(ckey))

	res := C.X_init(ccert, ckey)
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

func (ctx *SSLCtx) accept(poll *internal.Poll, fd int) (*SSL, error) {
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

func (s *SSL) Read(p []byte) (int, error) {
	if !s.handshaked {
		res := C.X_handshake(s.ssl)
		switch res {
		case 0:
			s.handshaked = true
		case C.X_SSL_ERROR_WANT_WRITE:
			return -1, fmt.Errorf("failed to handshake, TODO: retry write")
		case C.X_SSL_ERROR_WANT_READ:
		default:
			return -1, fmt.Errorf("failed to handshake: %v", sslGetError())
		}
		return -1, syscall.EAGAIN
	}
	rd := C.X_read(s.ssl, unsafe.Pointer(&p[0]), C.ulong(len(p)))
	if rd >= 0 {
		return int(rd), nil
	}
	if rd == C.X_SSL_ERROR_WANT_READ {
		return -1, syscall.EAGAIN
	}
	return -1, fmt.Errorf("failed to read: %v", sslGetError())
}

func (s *SSL) Write(p []byte) (int, error) {
	if !s.handshaked {
		return -1, fmt.Errorf("write before handshaking")
	}
	rd := C.X_write(s.ssl, unsafe.Pointer(&p[0]), C.ulong(len(p)))
	if rd >= 0 {
		return int(rd), nil
	}
	if rd == C.X_SSL_ERROR_WANT_WRITE {
		return int(rd), syscall.EAGAIN
	}
	return -1, fmt.Errorf("failed to write: %v", sslGetError())
}

func (s *SSL) Close() {
	C.X_close(s.ssl)
}
