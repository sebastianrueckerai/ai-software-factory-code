package x

type opts struct {
	Allow bool
}

func (o opts) on() bool { return o.Allow }
