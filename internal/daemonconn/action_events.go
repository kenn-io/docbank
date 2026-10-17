package daemonconn

// WithActionSurface returns a connection for an interactive import admission.
// Callers keep status and other background requests on their original connection.
func (c *Connection) WithActionSurface() *Connection {
	connection := *c
	connection.actionSurface = "cli"
	return &connection
}
