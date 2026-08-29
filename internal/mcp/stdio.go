package mcp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxStdioRequestBytes = 1 << 20

var (
	errInvalidStdioFrame = errors.New("invalid MCP stdio frame")
	errStdioTransport    = errors.New("MCP stdio transport failed")
)

// ServeStdio runs one exact-version server over newline-delimited stdin and
// stdout. Diagnostics contain only stable error codes; request data never
// crosses onto the diagnostics stream. The connection owns both streams;
// closing them must interrupt pending reads and writes.
func ServeStdio(
	ctx context.Context,
	server *Server,
	input io.ReadCloser,
	output io.WriteCloser,
	logger *slog.Logger,
) error {
	if server == nil || input == nil || output == nil {
		return errStdioTransport
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	connection := &stdioConnection{
		input: input, output: output,
		reader: bufio.NewReaderSize(input, maxStdioRequestBytes+2),
	}
	// SDK session shutdown waits for active calls before closing the transport.
	// Interrupt I/O first so a blocked response cannot hold that shutdown open.
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer func() {
		stop()
		_ = connection.Close()
	}()
	err := server.Run(ctx, connection)
	switch {
	case ctx.Err() != nil:
		return ctx.Err()
	case err == nil, errors.Is(err, io.EOF):
		return nil
	case errors.Is(err, errInvalidStdioFrame):
		logger.Error("MCP stdio stopped", "error_code", "invalid_frame")
		return errInvalidStdioFrame
	default:
		logger.Error("MCP stdio stopped", "error_code", "transport_failure")
		return errStdioTransport
	}
}

func (connection *stdioConnection) Connect(context.Context) (sdkmcp.Connection, error) {
	return connection, nil
}

func (*stdioConnection) SupportsProtocolVersion(version string) bool {
	return version == ProtocolVersion
}

type stdioConnection struct {
	input     io.ReadCloser
	reader    *bufio.Reader
	output    io.WriteCloser
	writeMu   sync.Mutex
	closeOnce sync.Once
	closeErr  error
}

func (connection *stdioConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	frame, err := connection.reader.ReadSlice('\n')
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if errors.Is(err, io.EOF) && len(frame) == 0 {
		return nil, io.EOF
	}
	if err != nil || len(frame) > maxStdioRequestBytes+2 || len(frame) == 0 || frame[len(frame)-1] != '\n' {
		return nil, errInvalidStdioFrame
	}
	frame = frame[:len(frame)-1]
	if len(frame) > 0 && frame[len(frame)-1] == '\r' {
		frame = frame[:len(frame)-1]
	}
	if len(frame) == 0 || len(frame) > maxStdioRequestBytes || bytes.ContainsAny(frame, "\r\n") {
		return nil, errInvalidStdioFrame
	}
	message, decodeErr := jsonrpc.DecodeMessage(frame)
	if decodeErr != nil {
		return nil, errInvalidStdioFrame
	}
	return message, nil
}

func (connection *stdioConnection) Write(ctx context.Context, message jsonrpc.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded, err := jsonrpc.EncodeMessage(message)
	if err != nil || bytes.ContainsAny(encoded, "\r\n") {
		return errStdioTransport
	}
	connection.writeMu.Lock()
	defer connection.writeMu.Unlock()
	encoded = append(encoded, '\n')
	for len(encoded) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		written, writeErr := connection.output.Write(encoded)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if writeErr != nil {
			return errStdioTransport
		}
		if written <= 0 || written > len(encoded) {
			return errStdioTransport
		}
		encoded = encoded[written:]
	}
	return nil
}

func (connection *stdioConnection) Close() error {
	connection.closeOnce.Do(func() {
		connection.closeErr = errors.Join(connection.input.Close(), connection.output.Close())
	})
	return connection.closeErr
}

func (*stdioConnection) SessionID() string { return "" }
