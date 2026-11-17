package x

import (
	"fmt"
	Str "strings"
)

var (
	EgressHosts = []string{"proxy.internal"}
	TimeoutSecs = 30
)

const (
	PrivilegedMode = false
	MaxPods        = 4
)

type (
	PodClass  int
	NodeLabel string
)

var UserA, userB string

type set[T comparable] struct{ m map[T]bool }

func (s *set[T]) Add(v T) { s.m[v] = true }

type Logger interface{ Log(string) }

type sink struct {
	Logger
	Hook func(token string) error
}

type reader interface {
	Logger
	read() string
}

func run[Elem any](xs []Elem) (Err error) {
	TokenA, tokenB := "a", "b"
	_ = TokenA + tokenB
Loop:
	for range xs {
		break Loop
	}
	return Err
}

func uses(s *set[int], k sink) {
	f := (*set[int]).Add
	f(s, 1)
	k.Hook = nil
	fmt.Println(Str.ToUpper("x"), EgressHosts, TimeoutSecs, PrivilegedMode, MaxPods, PodClass(1), NodeLabel("n"), UserA, userB)
}
