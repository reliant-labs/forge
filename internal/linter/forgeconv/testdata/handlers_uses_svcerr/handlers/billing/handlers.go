// Fixture: importing forge/pkg/svcerr suppresses the rule for this
// file. Migration in flight should not trip the warning.
//nolint:all
package billing

import (
	"errors"
	"fmt"

	"connectrpc.com/connect"

	"github.com/reliant-labs/forge/pkg/svcerr"
)

var ErrLegacy = errors.New("legacy")

// Even with a suspect-named helper still present, presence of the
// svcerr import marks the file as "in migration" and suppresses.
func toConnectError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, ErrLegacy):
		return connect.NewError(connect.CodeAborted, err)
	case errors.Is(err, svcerr.ErrNotFound):
		return svcerr.Wrap(err)
	default:
		return connect.NewError(connect.CodeInternal, fmt.Errorf("internal"))
	}
}
