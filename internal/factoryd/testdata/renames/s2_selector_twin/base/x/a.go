package x

type ciPolicy struct {
	egress []string
}

type agentPolicy struct {
	egress []string
	Egress []string
}

func rules(c ciPolicy, a agentPolicy) ([]string, []string) {
	return c.egress, a.egress
}
