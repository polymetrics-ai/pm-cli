package contract

// CheckFoundationCompatibility requires an exact installed behavioral revision for every reference.
// A future compatibility range requires an explicit versioned contract rather than an implicit semver guess.
func CheckFoundationCompatibility(required []FoundationRef, installed []FoundationContract) error {
	available := make(map[string]FoundationContract, len(installed))
	for _, foundation := range installed {
		if err := foundation.Validate(); err != nil {
			return contractError(ErrFoundationIncompatible, "installed_foundations", "contains an invalid foundation identity")
		}
		key := foundation.ID + "@" + foundation.Version
		if _, duplicate := available[key]; duplicate {
			return contractError(ErrFoundationIncompatible, "installed_foundations", "contains a duplicate foundation revision")
		}
		available[key] = foundation
	}

	visiting := make(map[string]struct{}, len(installed))
	verified := make(map[string]struct{}, len(installed))
	var verifyClosure func(string) error
	verifyClosure = func(key string) error {
		if _, ok := verified[key]; ok {
			return nil
		}
		if _, cycle := visiting[key]; cycle {
			return contractError(ErrFoundationIncompatible, "foundation_requirements", "foundation dependency closure contains a cycle")
		}
		foundation, ok := available[key]
		if !ok {
			return contractError(ErrFoundationIncompatible, "foundation_requirements", "foundation dependency revision is not installed")
		}
		visiting[key] = struct{}{}
		for _, dependency := range foundation.Dependencies {
			if err := verifyClosure(dependency.ID + "@" + dependency.Version); err != nil {
				return err
			}
		}
		delete(visiting, key)
		verified[key] = struct{}{}
		return nil
	}

	seen := make(map[string]struct{}, len(required))
	for _, reference := range required {
		if err := reference.Validate(); err != nil {
			return contractError(ErrFoundationIncompatible, "foundation_requirements", "contains an invalid foundation reference")
		}
		key := reference.ID + "@" + reference.Version
		if _, duplicate := seen[key]; duplicate {
			return contractError(ErrFoundationIncompatible, "foundation_requirements", "contains a duplicate foundation reference")
		}
		seen[key] = struct{}{}
		if _, ok := available[key]; !ok {
			return contractError(ErrFoundationIncompatible, "foundation_requirements", "required revision is not installed")
		}
		if err := verifyClosure(key); err != nil {
			return err
		}
	}
	return nil
}
