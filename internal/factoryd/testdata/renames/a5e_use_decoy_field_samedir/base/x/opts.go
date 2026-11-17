package x

type opts struct {
	allow bool
}

func (o opts) on() bool { return o.allow }
