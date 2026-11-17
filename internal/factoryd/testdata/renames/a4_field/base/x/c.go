package x

type creds struct {
	user  string
	token string
}

func header(c creds) string { return "Bearer " + c.token }
