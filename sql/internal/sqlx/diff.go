// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package sqlx

import (
	"fmt"
	"reflect"
	"sort"

	"ariga.io/atlas/sql/schema"
)

type (
	// A Diff provides a generic schema.Differ for diffing schema elements.
	//
	// The DiffDriver is required for supporting database/dialect specific
	// diff capabilities, like diffing custom types or attributes.
	Diff struct {
		DiffDriver
	}

	// A DiffDriver wraps all required methods for diffing elements that may
	// have database-specific diff logic. See sql/schema/mysql/diff.go for an
	// implementation example.
	DiffDriver interface {
		// SchemaAttrDiff returns a changeset for migrating schema attributes
		// from one state to the other. For example, changing schema collation.
		SchemaAttrDiff(from, to *schema.Schema) []schema.Change

		// TableAttrDiff returns a changeset for migrating table attributes from
		// one state to the other. For example, dropping or adding a `CHECK` constraint.
		TableAttrDiff(from, to *schema.Table) []schema.Change

		// ColumnChange returns the schema changes (if any) for migrating one column to the other.
		ColumnChange(from, to *schema.Column) (schema.ChangeKind, error)

		// IndexAttrChanged reports if the index attributes were changed.
		// For example, an index type or predicate (for partial indexes).
		IndexAttrChanged(from, to []schema.Attr) bool

		// IndexPartAttrChanged reports if the index-part attributes were
		// changed. For example, an index-part collation.
		IndexPartAttrChanged(from, to []schema.Attr) bool

		// ReferenceChanged reports if the foreign key referential action was
		// changed. For example, action was changed from RESTRICT to CASCADE.
		ReferenceChanged(from, to schema.ReferenceOption) bool
	}

	// A Normalizer wraps the Normalize method for normalizing table
	// elements that were inspected from the database, or were defined
	// by the users to a standard form.
	//
	// If the DiffDriver implements the Normalizer interface, TableDiff
	// normalizes its table inputs before starting the diff process.
	Normalizer interface {
		// Normalize normalizes a list of tables.
		Normalize(...*schema.Table)
	}
)

// SchemaDiff implements the schema.Differ interface and returns a list of
// changes that need to be applied in order to move from one state to the other.
func (d *Diff) SchemaDiff(from, to *schema.Schema) ([]schema.Change, error) {
	var changes []schema.Change
	// Drop or modify attributes (collations, checks, etc).
	changes = append(changes, d.SchemaAttrDiff(from, to)...)

	// Drop or modify tables.
	for _, t1 := range from.Tables {
		t2, ok := to.Table(t1.Name)
		if !ok {
			changes = append(changes, &schema.DropTable{T: t1})
			continue
		}
		change, err := d.TableDiff(t1, t2)
		if err != nil {
			return nil, err
		}
		if len(change) > 0 {
			changes = append(changes, &schema.ModifyTable{
				T:       t1,
				Changes: change,
			})
		}
	}
	// Add tables.
	for _, t1 := range to.Tables {
		if _, ok := from.Table(t1.Name); !ok {
			changes = append(changes, &schema.AddTable{T: t1})
		}
	}
	return changes, nil
}

// TableDiff implements the schema.TableDiffer interface and returns a list of
// changes that need to be applied in order to move from one state to the other.
func (d *Diff) TableDiff(from, to *schema.Table) ([]schema.Change, error) {
	// Normalizing tables before starting the diff process.
	if n, ok := d.DiffDriver.(Normalizer); ok {
		n.Normalize(from, to)
	}

	var changes []schema.Change
	if from.Name != to.Name {
		return nil, fmt.Errorf("mismatched table names: %q != %q", from.Name, to.Name)
	}
	// PK modification is not supported.
	if pk1, pk2 := from.PrimaryKey, to.PrimaryKey; (pk1 != nil) != (pk2 != nil) || (pk1 != nil) && d.pkChange(pk1, pk2) != schema.NoChange {
		return nil, fmt.Errorf("changing %q table primary key is not supported", to.Name)
	}

	// Drop or modify attributes (collations, checks, etc).
	changes = append(changes, d.TableAttrDiff(from, to)...)

	// Drop or modify columns.
	for _, c1 := range from.Columns {
		c2, ok := to.Column(c1.Name)
		if !ok {
			changes = append(changes, &schema.DropColumn{C: c1})
			continue
		}
		change, err := d.ColumnChange(c1, c2)
		if err != nil {
			return nil, err
		}
		if change != schema.NoChange {
			changes = append(changes, &schema.ModifyColumn{
				From:   c1,
				To:     c2,
				Change: change,
			})
		}
	}
	// Add columns.
	for _, c1 := range to.Columns {
		if _, ok := from.Column(c1.Name); !ok {
			changes = append(changes, &schema.AddColumn{C: c1})
		}
	}

	// Drop or modify indexes.
	for _, idx1 := range from.Indexes {
		idx2, ok := to.Index(idx1.Name)
		if !ok {
			changes = append(changes, &schema.DropIndex{I: idx1})
			continue
		}
		if change := d.indexChange(idx1, idx2); change != schema.NoChange {
			changes = append(changes, &schema.ModifyIndex{
				From:   idx1,
				To:     idx2,
				Change: change,
			})
		}
	}
	// Add indexes.
	for _, idx1 := range to.Indexes {
		if _, ok := from.Index(idx1.Name); !ok {
			changes = append(changes, &schema.AddIndex{I: idx1})
		}
	}

	// Drop or modify foreign-keys.
	for _, fk1 := range from.ForeignKeys {
		fk2, ok := to.ForeignKey(fk1.Symbol)
		if !ok {
			changes = append(changes, &schema.DropForeignKey{F: fk1})
			continue
		}
		if change := d.fkChange(fk1, fk2); change != schema.NoChange {
			changes = append(changes, &schema.ModifyForeignKey{
				From:   fk1,
				To:     fk2,
				Change: change,
			})
		}
	}
	// Add foreign-keys.
	for _, fk1 := range to.ForeignKeys {
		if _, ok := from.ForeignKey(fk1.Symbol); !ok {
			changes = append(changes, &schema.AddForeignKey{F: fk1})
		}
	}
	return changes, nil
}

// pkChange returns the schema changes (if any) for migrating one primary key to the other.
func (d *Diff) pkChange(from, to *schema.Index) schema.ChangeKind {
	change := d.indexChange(from, to)
	return change & ^schema.ChangeUnique
}

// indexChange returns the schema changes (if any) for migrating one index to the other.
func (d *Diff) indexChange(from, to *schema.Index) schema.ChangeKind {
	var change schema.ChangeKind
	if from.Unique != to.Unique {
		change |= schema.ChangeUnique
	}
	if d.IndexAttrChanged(from.Attrs, to.Attrs) {
		change |= schema.ChangeAttr
	}
	change |= d.partsChange(from.Parts, to.Parts)
	change |= CommentChange(from.Attrs, to.Attrs)
	return change
}

func (d *Diff) partsChange(from, to []*schema.IndexPart) schema.ChangeKind {
	if len(from) != len(to) {
		return schema.ChangeParts
	}
	sort.Slice(to, func(i, j int) bool { return to[i].SeqNo < to[j].SeqNo })
	sort.Slice(from, func(i, j int) bool { return from[i].SeqNo < from[j].SeqNo })
	for i := range from {
		switch {
		case d.IndexPartAttrChanged(from[i].Attrs, to[i].Attrs):
			return schema.ChangeParts
		case from[i].C != nil && to[i].C != nil:
			if from[i].C.Name != to[i].C.Name {
				return schema.ChangeParts
			}
		case from[i].X != nil && to[i].X != nil:
			if from[i].X.(*schema.RawExpr).X != to[i].X.(*schema.RawExpr).X {
				return schema.ChangeParts
			}
		default: // (C1 != nil) != (C2 != nil) || (X1 != nil) != (X2 != nil).
			return schema.ChangeParts
		}
	}
	return schema.NoChange
}

// fkChange returns the schema changes (if any) for migrating one index to the other.
func (d *Diff) fkChange(from, to *schema.ForeignKey) schema.ChangeKind {
	var change schema.ChangeKind
	switch {
	case from.Table.Name != to.Table.Name:
		change |= schema.ChangeRefTable | schema.ChangeRefColumn
	case len(from.RefColumns) != len(to.RefColumns):
		change |= schema.ChangeRefColumn
	default:
		for i := range from.RefColumns {
			if from.RefColumns[i].Name != to.RefColumns[i].Name {
				change |= schema.ChangeRefColumn
			}
		}
	}
	switch {
	case len(from.Columns) != len(to.Columns):
		change |= schema.ChangeColumn
	default:
		for i := range from.Columns {
			if from.Columns[i].Name != to.Columns[i].Name {
				change |= schema.ChangeColumn
			}
		}
	}
	if d.ReferenceChanged(from.OnUpdate, to.OnUpdate) {
		change |= schema.ChangeUpdateAction
	}
	if d.ReferenceChanged(from.OnDelete, to.OnDelete) {
		change |= schema.ChangeDeleteAction
	}
	return change
}

// CommentChange reports if the element comment was changed.
func CommentChange(from, to []schema.Attr) schema.ChangeKind {
	var c1, c2 schema.Comment
	if Has(from, &c1) != Has(to, &c2) || c1.Text != c2.Text {
		return schema.ChangeComment
	}
	return schema.NoChange
}

// Has finds the first attribute in the attribute list that
// matches target, and if so, sets target to that attribute
// value and returns true.
func Has(attrs []schema.Attr, target interface{}) bool {
	rv := reflect.ValueOf(target)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		panic("target must be a non-nil pointer")
	}
	for _, attr := range attrs {
		if reflect.TypeOf(attr).AssignableTo(rv.Type()) {
			rv.Elem().Set(reflect.ValueOf(attr).Elem())
			return true
		}
	}
	return false
}

// UnsupportedTypeError describes an unsupported type error.
type UnsupportedTypeError struct {
	schema.Type
}

func (e UnsupportedTypeError) Error() string {
	return fmt.Sprintf("unsupported type %T", e.Type)
}