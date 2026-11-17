package x

import (
	"fmt"
	str "strings"
)

var (
	egressHosts = []string{"proxy.internal"}
	timeoutSecs = 30
)

const (
	privilegedMode = false
	maxPods        = 4
)

type (
	podClass  int
	nodeLabel string
)

var userA, userB string

type set[T comparable] struct{ m map[T]bool }

func (s *set[T]) add(v T) { s.m[v] = true }

type logger interface{ Log(string) }

type sink struct {
	logger
	hook func(token string) error
}

type reader interface {
	logger
	read() string
}

func run[elem any](xs []elem) (err error) {
	tokenA, tokenB := "a", "b"
	_ = tokenA + tokenB
loop:
	for range xs {
		break loop
	}
	return err
}

func uses(s *set[int], k sink) {
	f := (*set[int]).add
	f(s, 1)
	k.hook = nil
	fmt.Println(str.ToUpper("x"), egressHosts, timeoutSecs, privilegedMode, maxPods, podClass(1), nodeLabel("n"), userA, userB)
}
