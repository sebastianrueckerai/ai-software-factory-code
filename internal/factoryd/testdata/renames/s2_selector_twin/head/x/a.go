package x

type ciPolicy struct {
	Egress []string
}

type agentPolicy struct {
	egress []string
	Egress []string
}

func rules(c ciPolicy, a agentPolicy) ([]string, []string) {
	return c.Egress, a.Egress
}
