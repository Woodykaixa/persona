package assistant

import (
	"context"

	"github.com/nwverse/persona/server/internal/assistantwire"
	"github.com/nwverse/persona/server/internal/kb"
	"github.com/nwverse/persona/server/internal/modules/knowledge-ui/kbsent"
)

// kbDaySent is the knowledge base day p names, with the lines the user sent from Persona marked (kbsent).
func (s *Service) kbDaySent(ctx context.Context, e *kb.Engine, p assistantwire.KBDayParams) (assistantwire.KBDayResult, error) {
	return kbsent.Day(ctx, s.store(), dayStart, e, p)
}
