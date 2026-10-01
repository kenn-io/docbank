package store

import (
	"context"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
)

// metadataRecordCodec is one registered backup record kind.
type metadataRecordCodec interface {
	kind() string
	fields() (required []string, nullable map[string]bool)
	// sqlTable names the kind's own table for the pristine-target check; it is
	// empty for a registration whose hooks own every table they write.
	sqlTable() string
	export(ctx context.Context, q metadataQuerier, write metadataWrite) error
	validateRows(ctx context.Context, q metadataQuerier) error
	importRecord(ctx context.Context, tx *sql.Tx, raw jsontext.Value) error
}

// metadataTable describes one backup record kind by its struct tags. A regular
// kind is declared by `db` tags plus a registration: `json` order is wire order,
// a `db:"column"` tag names the column (`db:"column,json"` stores the field as
// JSON text), and pointer fields are nullable. An `omitempty` field is outside
// the required and nullable lists, so an import line that carries it is refused
// as an unknown field. A `json:"-"` field with a `db` tag is a constant column
// set by the template. Irregular kinds set hooks and keep their own code.
type metadataTable[T any] struct {
	record      T      // template: Type plus constant fields; import decodes into a copy
	table       string // empty only when insert is set
	suffix      string // SQL after the table name: alias, WHERE, ORDER BY
	validate    func(T) error
	checkExport bool // the kind's exporter validated each row before writing it
	decode      func(jsontext.Value, *T) error
	exportAll   func(context.Context, metadataQuerier, metadataWrite) error
	insert      func(context.Context, *sql.Tx, T) error
	name        string
	plan        *metadataColumnPlan
}

type metadataColumn struct {
	index int
	name  string
	json  bool
}

type metadataColumnPlan struct {
	columns  []metadataColumn
	list     string
	required []string
	nullable map[string]bool
}

var (
	metadataColumnPlans sync.Map
	jsontextValueType   = reflect.TypeFor[jsontext.Value]()
)

func newMetadataTable[T any](spec metadataTable[T]) *metadataTable[T] {
	template := reflect.ValueOf(spec.record)
	typeField := template.FieldByName("Type")
	if !typeField.IsValid() || typeField.String() == "" {
		panic(fmt.Sprintf("metadata record %s lacks a template type", template.Type()))
	}
	if spec.table == "" && spec.insert == nil {
		panic("metadata record " + typeField.String() + " has neither a table nor an insert hook")
	}
	spec.name = typeField.String()
	spec.plan = metadataColumnPlanFor(template.Type())
	return &spec
}

func metadataColumnPlanFor(recordType reflect.Type) *metadataColumnPlan {
	if cached, ok := metadataColumnPlans.Load(recordType); ok {
		if plan, ok := cached.(*metadataColumnPlan); ok {
			return plan
		}
	}
	plan := &metadataColumnPlan{nullable: map[string]bool{}}
	names := make([]string, 0, recordType.NumField())
	for index := range recordType.NumField() {
		field := recordType.Field(index)
		if column, ok := field.Tag.Lookup("db"); ok {
			name, option, _ := strings.Cut(column, ",")
			if slices.Contains(names, name) || (option != "" && option != "json") {
				panic(fmt.Sprintf("metadata record %s has an invalid db tag %q", recordType, column))
			}
			names = append(names, name)
			plan.columns = append(plan.columns, metadataColumn{index: index, name: name, json: option == "json"})
		}
		wire, options, _ := strings.Cut(field.Tag.Get("json"), ",")
		if wire == "" || wire == "-" || strings.Contains(options, "omitempty") {
			continue
		}
		plan.required = append(plan.required, wire)
		if field.Type.Kind() == reflect.Pointer {
			plan.nullable[wire] = true
		}
	}
	plan.list = strings.Join(names, ",")
	metadataColumnPlans.Store(recordType, plan)
	return plan
}

func (t *metadataTable[T]) kind() string { return t.name }

func (t *metadataTable[T]) sqlTable() string { return t.table }

func (t *metadataTable[T]) fields() (required []string, nullable map[string]bool) {
	return t.plan.required, t.plan.nullable
}

func (t *metadataTable[T]) export(ctx context.Context, q metadataQuerier, write metadataWrite) error {
	if t.exportAll != nil {
		return t.exportAll(ctx, q, write)
	}
	return t.scan(ctx, q, t.checkExport, write)
}

// validateRows runs the kind's validator over every stored row and writes nothing.
func (t *metadataTable[T]) validateRows(ctx context.Context, q metadataQuerier) error {
	return t.scan(ctx, q, true, nil)
}

func (t *metadataTable[T]) scan(ctx context.Context, q metadataQuerier, check bool, write metadataWrite) error {
	words := strings.ReplaceAll(t.name, "_", " ")
	if t.table == "" {
		return fmt.Errorf("exporting %s metadata: the record is import-only", words)
	}
	rows, err := q.QueryContext(ctx, `SELECT `+t.plan.list+` FROM `+t.table+` `+t.suffix)
	if err != nil {
		return fmt.Errorf("exporting %s metadata: %w", words, err)
	}
	defer func() { _ = rows.Close() }()
	targets := make([]any, len(t.plan.columns))
	holders := make([]string, len(t.plan.columns))
	for rows.Next() {
		record := t.record
		value := reflect.ValueOf(&record).Elem()
		for position, column := range t.plan.columns {
			if column.json {
				targets[position] = &holders[position]
			} else {
				targets[position] = value.Field(column.index).Addr().Interface()
			}
		}
		if err := rows.Scan(targets...); err != nil {
			return fmt.Errorf("scanning %s metadata: %w", words, err)
		}
		for position, column := range t.plan.columns {
			field := value.Field(column.index)
			switch {
			case !column.json:
			case field.Type() == jsontextValueType:
				field.SetBytes([]byte(holders[position]))
			default:
				if err := json.Unmarshal([]byte(holders[position]), field.Addr().Interface()); err != nil {
					return fmt.Errorf("decoding %s %s: %w", words, column.name, err)
				}
			}
		}
		if check && t.validate != nil {
			if err := t.validate(record); err != nil {
				return fmt.Errorf("validating %s metadata for export: %w", words, err)
			}
		}
		if write != nil {
			if err := write(record); err != nil {
				return err
			}
		}
	}
	return rowsError(words, rows)
}

func (t *metadataTable[T]) importRecord(ctx context.Context, tx *sql.Tx, raw jsontext.Value) error {
	value := t.record
	decode := t.decode
	if decode == nil {
		decode = func(raw jsontext.Value, value *T) error { return decodeMetadataRecord(raw, value) }
	}
	if err := decode(raw, &value); err != nil {
		return err
	}
	if t.validate != nil {
		if err := t.validate(value); err != nil {
			return err
		}
	}
	if t.insert != nil {
		return t.insert(ctx, tx, value)
	}
	return insertMetadataRecord(ctx, tx, t.table, value)
}

// insertMetadataRecord writes every `db`-tagged field of record into table.
func insertMetadataRecord(ctx context.Context, tx *sql.Tx, table string, record any) error {
	value := reflect.ValueOf(record)
	plan := metadataColumnPlanFor(value.Type())
	args := make([]any, len(plan.columns))
	for position, column := range plan.columns {
		field := value.Field(column.index)
		switch {
		case !column.json:
			args[position] = field.Interface()
		case field.Type() == jsontextValueType:
			args[position] = string(field.Bytes())
		default:
			args[position] = mustCatalogJSON(field.Interface())
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+table+`(`+plan.list+`) VALUES(`+placeholders(len(args))+`)`, args...)
	return err
}

// exportMetadataTables exports each kind in slice order.
func exportMetadataTables(ctx context.Context, q metadataQuerier, write metadataWrite, tables []metadataRecordCodec) error {
	for _, table := range tables {
		if err := table.export(ctx, q, write); err != nil {
			return err
		}
	}
	return nil
}

func indexMetadataCodecs(groups ...[]metadataRecordCodec) map[string]metadataRecordCodec {
	codecs := map[string]metadataRecordCodec{}
	for _, group := range groups {
		for _, codec := range group {
			if _, ok := codecs[codec.kind()]; ok {
				panic("metadata record " + codec.kind() + " is registered twice")
			}
			codecs[codec.kind()] = codec
		}
	}
	return codecs
}

func metadataFieldMaps(codecs map[string]metadataRecordCodec) (map[string][]string, map[string]map[string]bool) {
	required := make(map[string][]string, len(codecs))
	nullable := map[string]map[string]bool{}
	for kind, codec := range codecs {
		fields, nullableFields := codec.fields()
		required[kind] = fields
		if len(nullableFields) > 0 {
			nullable[kind] = nullableFields
		}
	}
	return required, nullable
}

// pristineMetadataTableSum adds up every table a backup record restores into,
// plus the state tables no record restores. nodes and processing_incarnations
// start with bootstrap rows, so requirePristineMetadataTarget compares them to
// those rows instead.
func pristineMetadataTableSum(codecs map[string]metadataRecordCodec) string {
	tables := slices.Clone(metadataPristineStateTables)
	for _, codec := range codecs {
		if table := codec.sqlTable(); table != "" && table != "nodes" && table != "processing_incarnations" {
			tables = append(tables, table)
		}
	}
	slices.Sort(tables)
	tables = slices.Compact(tables)
	terms := make([]string, len(tables))
	for index, table := range tables {
		terms[index] = `(SELECT COUNT(*) FROM ` + table + `)`
	}
	return strings.Join(terms, " + ")
}

var metadataCodecs = indexMetadataCodecs(
	coreMetadataTables, emailMetadataTables, mailboxMetadataTables, pageMetadataTables,
	packageMetadataTables, packageImportMetadataTables, batesMetadataTables, photoMetadataTables,
	personMetadataTables, processingMetadataTables, embeddingMetadataTables, auditMetadataTables,
	[]metadataRecordCodec{
		emailDocumentPublicationMetadata, exportAuthorityMetadata,
		currentRenditionRootMetadata, derivativePurgeSuppressionMetadata,
	},
)

// Media's eight kinds merge in from media_metadata.go's init until #733 converts them.
var metadataRequiredFields, metadataNullableFields = metadataFieldMaps(metadataCodecs)
