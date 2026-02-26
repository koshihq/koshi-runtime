package provider

// GoogleParser is a stub for Google/Gemini provider support.
// Not implemented in v1.
type GoogleParser struct{}

// ParseUsage returns ErrNotImplemented for v1.
func (p *GoogleParser) ParseUsage(_ []byte) (*UsageData, error) {
	return nil, ErrNotImplemented
}
