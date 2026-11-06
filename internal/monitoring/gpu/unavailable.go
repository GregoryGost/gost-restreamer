package gpu

type unavailableProvider struct{ reason Reason }

func (p unavailableProvider) Init() Reason       { return p.reason }
func (p unavailableProvider) Poll(Limits) Sample { return Sample{Reason: p.reason} }
func (p unavailableProvider) Shutdown() Reason   { return Available }
