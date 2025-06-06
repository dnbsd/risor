package pgx

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/risor-io/risor/arg"
	"github.com/risor-io/risor/errz"
	"github.com/risor-io/risor/object"
	"github.com/risor-io/risor/op"
)

const PGX_CONN = object.Type("pgx.conn")

type PgxConn struct {
	ctx    context.Context
	conn   *pgx.Conn
	once   sync.Once
	closed chan bool
	stream bool // Whether to use streaming for query results
}

func (c *PgxConn) Type() object.Type {
	return PGX_CONN
}

func (c *PgxConn) Inspect() string {
	return "pgx.conn()"
}

func (c *PgxConn) Interface() interface{} {
	return c.conn
}

func (c *PgxConn) Value() *pgx.Conn {
	return c.conn
}

func (c *PgxConn) Equals(other object.Object) object.Object {
	return object.NewBool(c == other)
}

func (c *PgxConn) IsTruthy() bool {
	return true
}

func (c *PgxConn) GetAttr(name string) (object.Object, bool) {
	switch name {
	case "query":
		return object.NewBuiltin("pgx.conn.query", c.Query), true
	case "exec", "execute": // "exec" for backwards compatibility
		return object.NewBuiltin("pgx.conn.execute", c.Exec), true
	case "close":
		return object.NewBuiltin("pgx.conn.close", func(ctx context.Context, args ...object.Object) object.Object {
			if err := arg.Require("pgx.conn.close", 0, args); err != nil {
				return err
			}
			if err := c.Close(); err != nil {
				return object.NewError(err)
			}
			return object.Nil
		}), true
	}
	return nil, false
}

func (c *PgxConn) SetAttr(name string, value object.Object) error {
	return object.TypeErrorf("type error: pgx.conn object has no attribute %q", name)
}

func (c *PgxConn) RunOperation(opType op.BinaryOpType, right object.Object) object.Object {
	return object.TypeErrorf("type error: unsupported operation for pgx.conn: %v", opType)
}

func (c *PgxConn) Close() error {
	var err error
	c.once.Do(func() {
		err = c.conn.Close(c.ctx)
		close(c.closed)
	})
	return err
}

func (c *PgxConn) waitToClose() {
	go func() {
		select {
		case <-c.closed:
		case <-c.ctx.Done():
			c.conn.Close(c.ctx)
		}
	}()
}

func (c *PgxConn) Cost() int {
	return 8
}

func (c *PgxConn) MarshalJSON() ([]byte, error) {
	return nil, errz.TypeErrorf("type error: unable to marshal pgx.conn")
}

func New(ctx context.Context, conn *pgx.Conn, stream bool) *PgxConn {
	obj := &PgxConn{
		ctx:    ctx,
		conn:   conn,
		closed: make(chan bool),
		stream: stream,
	}
	obj.waitToClose()
	return obj
}

func (c *PgxConn) Query(ctx context.Context, args ...object.Object) object.Object {
	// The arguments should include a query string and zero or more query args
	if len(args) < 1 {
		return object.TypeErrorf("type error: pgx.conn.query() one or more arguments (%d given)", len(args))
	}
	query, errObj := object.AsString(args[0])
	if errObj != nil {
		return errObj
	}

	// Build list of query args as their Go types
	var queryArgs []interface{}
	for _, queryArg := range args[1:] {
		queryArgs = append(queryArgs, queryArg.Interface())
	}

	// Start the query
	rows, err := c.conn.Query(ctx, query, queryArgs...)
	if err != nil {
		return object.NewError(err)
	}

	// If streaming is enabled, return a row iterator
	if c.stream {
		return NewRowIterator(ctx, rows)
	}

	// Otherwise, process all rows and return a list (original behavior).
	// This loads all rows into memory at once, which can be problematic for
	// large result sets.
	defer rows.Close()

	// The field descriptions will tell us how to decode the result values
	fields := rows.FieldDescriptions()
	var results []object.Object

	// Transform each result row into a Risor map object
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return object.NewError(err)
		}
		row := map[string]object.Object{}
		for colIndex, value := range values {
			key := fields[colIndex].Name
			var val object.Object
			if timeVal, ok := value.(pgtype.Time); ok {
				usec := timeVal.Microseconds
				val = object.FromGoType(usec)
			} else {
				val = object.FromGoType(value)
			}
			if val == nil {
				return object.TypeErrorf("type error: pgx.conn.query() encountered unsupported type: %T", value)
			}
			if !object.IsError(val) {
				row[key] = val
			} else {
				row[key] = object.NewString(fmt.Sprintf("__error__%s", value))
			}
		}
		results = append(results, object.NewMap(row))
	}
	return object.NewList(results)
}

func (c *PgxConn) Exec(ctx context.Context, args ...object.Object) object.Object {
	if len(args) < 1 {
		return object.TypeErrorf("type error: pgx.conn.exec() one or more arguments (%d given)", len(args))
	}
	query, errObj := object.AsString(args[0])
	if errObj != nil {
		return errObj
	}
	var queryArgs []interface{}
	if len(args) == 2 {
		if list, ok := args[1].(*object.List); ok {
			for _, item := range list.Value() {
				queryArgs = append(queryArgs, item.Interface())
			}
		} else {
			queryArgs = append(queryArgs, args[1].Interface())
		}
	} else {
		for _, queryArg := range args[1:] {
			queryArgs = append(queryArgs, queryArg.Interface())
		}
	}
	commandTag, err := c.conn.Exec(ctx, query, queryArgs...)
	if err != nil {
		return object.NewError(err)
	}
	return object.NewString(commandTag.String())
}
