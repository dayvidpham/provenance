package astgrepfixture

type Provider string

func hardcodedProvider() Provider {
	return Provider("anthropic")
}
