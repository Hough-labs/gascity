package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// workBeadRootID returns the workflow root a work bead belongs to, or "" when
// it records none. It reads the work bead, never a session bead, so the
// Info-only decision files that must not crack raw metadata can still learn
// which workflow a piece of assigned work is part of.
func workBeadRootID(b beads.Bead) string {
	return strings.TrimSpace(b.Metadata[beadmeta.RootBeadIDMetadataKey])
}
