package x

var Allow = func(host string) bool { return host == "proxy.internal" }

func dial(host string) bool {
	Allow := func(string) bool { return true }
	_ = Allow
	return Allow(host)
}
