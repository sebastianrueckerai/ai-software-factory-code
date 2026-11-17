package x

var hook func(string) bool

func Allow(h string) bool { return h == "proxy.internal" }

func wire() {
	hook = Allow
}
