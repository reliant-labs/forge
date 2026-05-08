package orm

// Model is the interface that generated ORM types must implement.
// All protobuf messages with ORM annotations will automatically implement this interface.
type Model interface {
	// TableName returns the database table name for this model
	TableName() string

	// Schema returns the complete table schema including fields and indexes
	Schema() TableSchema

	// PrimaryKey returns the value of the primary key field
	PrimaryKey() any

	// Values returns the column names and values for this model in order
	Values() (columns []string, values []any)
}

// Scanner is implemented by generated types to scan database rows into themselves.
// This is used by Get and List operations.
type Scanner interface {
	Scan(scanner interface{ Scan(...interface{}) error }) error
}
