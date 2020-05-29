package spider

import (
	"fmt"
)

// SavePageError is an error used to signal that there are no jobs in the storage
type SavePageError struct {
	Err string
}

func (e *SavePageError) Error() string {
	return fmt.Sprintf(e.Err)
}
