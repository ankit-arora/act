// +build !linux,!darwin,!windows,!openbsd

package container

// NewContainer creates a reference to a container
func NewContainer(input *NewContainerInput) Container {
	return nil
}
