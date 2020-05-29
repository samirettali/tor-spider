package spider

import (
	"fmt"
)

// NoJobsError is an error used to signal that there are no jobs in the storage
type NoJobsError struct {
	Err string
}

func (e *NoJobsError) Error() string {
	return fmt.Sprintf(e.Err)
}
