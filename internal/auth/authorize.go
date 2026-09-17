package auth

import (
	"errors"
)

type InboundRequest struct {
	TLS                     bool
	Host                    string
	Origin                  string
	Browser                 bool
	Mutation                bool
	RequireOrigin           bool
	BearerCredentialInQuery bool
	Loopback                bool
}

// ValidateInboundPolicy applies the same exact TLS, Host, Origin and credential
// channel rules to loopback and remote requests. It performs no application I/O.
func (service *Service) ValidateInboundPolicy(request InboundRequest) error {
	if service == nil {
		return ErrInvalidInboundPolicy
	}
	if !request.TLS || !validExactHost(request.Host) ||
		!containsExact(service.inbound.AllowedHosts, request.Host) || request.BearerCredentialInQuery {
		return ErrInvalidInboundPolicy
	}
	if !request.Browser {
		if request.Origin != "" {
			return ErrInvalidInboundPolicy
		}
		return nil
	}
	if request.Mutation || request.RequireOrigin {
		if request.Origin == "" || !containsExact(service.inbound.AllowedOrigins, request.Origin) {
			return ErrInvalidInboundPolicy
		}
		return nil
	}
	if request.Origin != "" && !containsExact(service.inbound.AllowedOrigins, request.Origin) {
		return ErrInvalidInboundPolicy
	}
	return nil
}

func IsSafeScopeDenial(err error) bool {
	return errors.Is(err, ErrResourceNotFound) || errors.Is(err, ErrAuthorizationDenied) ||
		errors.Is(err, ErrGrantUnavailable) || errors.Is(err, ErrDelegationInvalid)
}
