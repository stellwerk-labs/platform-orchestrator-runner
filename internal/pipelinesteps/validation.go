package steps

// ValidateDefinition checks the frozen local executable without starting a task.
func ValidateDefinition(def Definition) error { return validateDefinition(def) }

// ValidateEnvironment checks explicit task values before a rollout is accepted.
func ValidateEnvironment(env map[string]string) error { return validateEnvironment(env) }
