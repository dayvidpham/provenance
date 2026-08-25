package astgrepfixture

func registersBareModelName(ns, role, provider any) {
	RegisterMLAgent(ns, role, provider, "claude-opus-4-6")
}

func RegisterMLAgent(ns, role, provider any, model string) {}
