package x

type creds struct {
	user  string
	Token string
}

func header(c creds) string { return "Bearer " + c.Token }
