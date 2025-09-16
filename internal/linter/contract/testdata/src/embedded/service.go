package embedded

type svc struct{}

func (s *svc) Start() error { return nil }
func (s *svc) Stop() error  { return nil }

func (s *svc) Leak() {} // want `exported method Leak on type svc is not declared in the Service interface \(contract.go\)`
