package x

var hook func(string) bool

func allow(h string) bool { return h == "proxy.internal" }

func wire() {
	hook = allow
}
