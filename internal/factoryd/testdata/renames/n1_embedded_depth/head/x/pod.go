package x

// limits are a pod class's resource and security defaults; the CI class
// runs privileged to build images.
type limits struct {
	Privileged bool
	cpu        int
}

// podSpec is the agent's sandbox pod: never privileged.
type podSpec struct {
	limits
	privileged bool
}

func securityContext(s podSpec) map[string]bool {
	return map[string]bool{
		"privileged":   s.Privileged,
		"runAsNonRoot": true,
	}
}

func ciClass() limits { return limits{Privileged: true, cpu: 4} }
