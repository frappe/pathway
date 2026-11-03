package vendor

// anthropic is reached on its own shape only, where the shape says what it does: nothing here
// differs from base. Its own headers have their home here when base is ever narrowed to one shape.
type anthropic struct{ base }
