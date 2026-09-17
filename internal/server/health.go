package server

import "context"

// Readiness contains only safe process capability state, never dependency
// addresses, credentials, tenant catalogs or raw dependency errors.
type Readiness struct {
	Admission bool `json:"admission"`
	Dispatch  bool `json:"dispatch"`
	Draining  bool `json:"draining"`
}

func (server *Server) Readiness(ctx context.Context) Readiness {
	server.mu.Lock()
	state := server.state
	server.mu.Unlock()
	if ctx == nil || state != stateServing {
		return Readiness{Draining: state == stateDraining || state == stateStopped}
	}
	probeContext, cancel := context.WithTimeout(ctx, server.settings.ProbeTimeout)
	defer cancel()
	result := Readiness{
		Admission: server.admissionProbe(probeContext) == nil,
	}
	if server.dispatchProbe != nil {
		result.Dispatch = server.dispatchProbe(probeContext) == nil
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.state != stateServing {
		return Readiness{Draining: server.state == stateDraining || server.state == stateStopped}
	}
	return result
}
