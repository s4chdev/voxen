package daemon_test

import (
	"encoding/json"
	"errors"

	"github.com/s4chdev/voxen/internal/ipc"
)

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

func asIPCError(err error, target **ipc.Error) bool { return errors.As(err, target) }
