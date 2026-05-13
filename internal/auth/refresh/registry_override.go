package refresh

// tokenURLOverrides lets tests redirect refresh requests at httptest mock
// servers without touching the global providers.Providers map (which is
// also read by the live OAuth flow code and shouldn't be mutated).
//
// In production this map is empty. Tests populate it via setTokenURLOverride
// and clean up with a deferred unset.
var tokenURLOverrides = map[string]string{}
