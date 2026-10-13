//go:build !darwin

package source

import (
	"context"
	"errors"
)

const bulkSupported = false

func listBulk(context.Context, string, bool) ([]Entry, error) {
	return nil, errors.ErrUnsupported
}
