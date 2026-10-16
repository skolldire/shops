package domain

import "errors"

var ErrReservationMismatch = errors.New("ordering: the reserved stock does not match the request")
