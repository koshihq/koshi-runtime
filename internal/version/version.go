package version

// Version is the current Koshi version. Override at build time via:
//
//	go build -ldflags "-X github.com/koshihq/koshi-runtime/internal/version.Version=v1.0.0"
var Version = "v1.0.0"
