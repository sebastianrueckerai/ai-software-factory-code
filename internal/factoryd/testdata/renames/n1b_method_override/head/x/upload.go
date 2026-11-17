package x

// upload sends the agent's artifact through the sandbox's transport.
func upload(g guarded, host string) error {
	return g.Send(host)
}
