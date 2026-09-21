//go:build windows

package store

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	crypt32            = syscall.NewLazyDLL("crypt32.dll")
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procProtectData    = crypt32.NewProc("CryptProtectData")
	procUnprotectData  = crypt32.NewProc("CryptUnprotectData")
	procLocalFree      = kernel32.NewProc("LocalFree")
	cryptProtectUINone = uintptr(0x1) // CRYPTPROTECT_UI_FORBIDDEN
)

// entropy binds the blob to this app, so a DPAPI blob lifted from another secret
// stored by the same Windows user will not unlock here.
var entropy = []byte("mimo-switch.credential.v1")

type dataBlob struct {
	size uint32
	ptr  *byte
}

func newBlob(b []byte) dataBlob {
	if len(b) == 0 {
		return dataBlob{}
	}
	return dataBlob{size: uint32(len(b)), ptr: &b[0]}
}

func (b dataBlob) bytes() []byte {
	if b.size == 0 || b.ptr == nil {
		return nil
	}
	return unsafe.Slice(b.ptr, b.size)
}

func protect(plain []byte) ([]byte, error) {
	in, ent := newBlob(plain), newBlob(entropy)
	var out dataBlob
	ok, _, errno := procProtectData.Call(
		uintptr(unsafe.Pointer(&in)), 0,
		uintptr(unsafe.Pointer(&ent)), 0, 0,
		cryptProtectUINone,
		uintptr(unsafe.Pointer(&out)),
	)
	if ok == 0 {
		return nil, fmt.Errorf("CryptProtectData: %w", errno)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.ptr)))
	return append([]byte(nil), out.bytes()...), nil
}

func unprotect(blob []byte) ([]byte, error) {
	in, ent := newBlob(blob), newBlob(entropy)
	var out dataBlob
	ok, _, errno := procUnprotectData.Call(
		uintptr(unsafe.Pointer(&in)), 0,
		uintptr(unsafe.Pointer(&ent)), 0, 0,
		cryptProtectUINone,
		uintptr(unsafe.Pointer(&out)),
	)
	if ok == 0 {
		return nil, fmt.Errorf("CryptUnprotectData: %w", errno)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.ptr)))
	return append([]byte(nil), out.bytes()...), nil
}
