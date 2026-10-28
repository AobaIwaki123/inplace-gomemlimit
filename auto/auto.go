package auto

import (
	"context"

	"github.com/AobaIwaki123/inplace-gomemlimit/pkg/watcher"
)

func init() {
	w := watcher.New(watcher.Options{})
	_ = w.Start(context.Background())
}
