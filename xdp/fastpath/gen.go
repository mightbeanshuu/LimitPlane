package fastpath

// bpf2go compiles ../bpf/fastpath.c with clang and embeds the object, plus
// typed Go accessors for its maps and program, into fastpath_bpfel.go.
// The output is committed, so building the agent needs Go, not clang.
//
//go:generate go tool bpf2go -target bpfel -go-package fastpath fastpath ../bpf/fastpath.c -- -O2 -g -Wall -I/usr/include/aarch64-linux-gnu -I/usr/include/x86_64-linux-gnu
