package source

import (
	"fmt"

	"github.com/Wlczak/aws-backup/internal/config"
)

// FromConfig returns a Source matching cfg.Type. It's the single place
// the CLI and API agree on how SourceConfig maps to a live Source.
func FromConfig(cfg config.SourceConfig) (Source, error) {
	switch cfg.Type {
	case config.SourceLocalDir:
		return NewLocalDir(cfg.LocalDir.Root)
	case config.SourceSMB:
		return NewSMB(SMBConfig{
			Host:     cfg.SMB.Host,
			Port:     cfg.SMB.Port,
			Username: cfg.SMB.Username,
			Password: cfg.SMB.Password,
			Domain:   cfg.SMB.Domain,
			Share:    cfg.SMB.Share,
			Path:     cfg.SMB.Path,
		})
	default:
		return nil, fmt.Errorf("source: unknown type %q", cfg.Type)
	}
}
