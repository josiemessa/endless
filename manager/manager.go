package manager

import (
	"github.com/stevenh/endless"
)

// Manager is a
type Manager interface {
	Manage(srv *endless.Server) error
	Stop()
}
