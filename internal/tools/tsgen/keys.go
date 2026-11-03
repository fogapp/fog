package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"strconv"
	"strings"
)

// keySet is a root that is a map, not a struct: Var, a package-level map
// literal in Path, whose keys are the JSON object's string fields. Secret,
// when set, names a map in the same package whose keys are write-only: the
// handler answers "" for each and adds a read-only <key>_set that is "on"
// when a value is stored (fogd's handleGetSettings).
type keySet struct {
	Path, Var, TS string
	Secret        string
}

// keySet renders one key set as an interface of string fields, in the
// map's source order. A comment on an entry (the line above it, or after
// it on its first line) carries over to its field.
func (g *gen) keySet(ks keySet) error {
	path := modulePath + "/" + ks.Path
	p, err := g.load(path)
	if err != nil {
		return err
	}
	if other, ok := g.names[ks.TS]; ok {
		return fmt.Errorf("%s.%s and %s would both be %s", path, ks.Var, other, ks.TS)
	}
	g.names[ks.TS] = typeKey{path, ks.Var}
	entries, err := g.mapEntries(p, ks.Var)
	if err != nil {
		return err
	}
	keys := map[string]bool{}
	for _, e := range entries {
		if keys[e.key] {
			return fmt.Errorf("%s: %q is a key twice", ks.Var, e.key)
		}
		keys[e.key] = true
	}
	secret := map[string]bool{}
	if ks.Secret != "" {
		secrets, err := g.mapEntries(p, ks.Secret)
		if err != nil {
			return err
		}
		for _, e := range secrets {
			if !keys[e.key] {
				return fmt.Errorf("%s names %q, which %s does not", ks.Secret, e.key, ks.Var)
			}
			if keys[e.key+"_set"] {
				return fmt.Errorf("%s: %q collides with the read-only flag of %q", ks.Var, e.key+"_set", e.key)
			}
			secret[e.key] = true
		}
	}
	var b strings.Builder
	writeComment(&b, p.docs[ks.Var], "")
	fmt.Fprintf(&b, "export interface %s {\n", ks.TS)
	for _, e := range entries {
		writeComment(&b, e.doc, "  ")
		trailing := e.comment
		if secret[e.key] {
			const note = `write-only: GET always answers ""`
			if trailing == "" {
				trailing = note
			} else {
				trailing += " (" + note + ")"
			}
		}
		fmt.Fprintf(&b, "  %s: string;", fieldKey(e.key))
		if trailing != "" {
			fmt.Fprintf(&b, " // %s", trailing)
		}
		b.WriteString("\n")
		if secret[e.key] {
			fmt.Fprintf(&b, "  %s: string; // read-only: \"on\" when %s is stored\n", fieldKey(e.key+"_set"), e.key)
		}
	}
	b.WriteString("}\n")
	g.decls = append(g.decls, b.String())
	return nil
}

func fieldKey(name string) string {
	if isIdent(name) {
		return name
	}
	return strconv.Quote(name)
}

type mapEntry struct {
	key     string
	doc     *ast.CommentGroup // the comment on the lines above the entry
	comment string            // the comment after it, on its first line
}

// mapEntries reads the keys of a package-level map literal, resolving
// constant keys (local or imported) to their values.
func (g *gen) mapEntries(p *pkg, name string) ([]mapEntry, error) {
	vs, ok := p.vars[name]
	if !ok {
		return nil, fmt.Errorf("%s.%s: no such var", p.path, name)
	}
	var lit *ast.CompositeLit
	for i, n := range vs.Names {
		if n.Name == name && i < len(vs.Values) {
			lit, _ = vs.Values[i].(*ast.CompositeLit)
		}
	}
	if lit == nil {
		return nil, fmt.Errorf("%s.%s is not a map literal", p.path, name)
	}
	if _, ok := lit.Type.(*ast.MapType); !ok {
		return nil, fmt.Errorf("%s.%s is not a map literal", p.path, name)
	}
	f := p.files[vs]
	line := func(pos token.Pos) int { return p.fset.Position(pos).Line }
	var out []mapEntry
	prevEnd := lit.Lbrace
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			return nil, fmt.Errorf("%s.%s: entry without a key", p.path, name)
		}
		key, err := g.constString(p, f, kv.Key)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", p.path, name, err)
		}
		e := mapEntry{key: key}
		for _, cg := range f.Comments {
			switch {
			// Both start below the previous entry's last line, which a
			// trailing comment of that entry does not.
			case line(cg.Pos()) > line(prevEnd) && cg.End() < kv.Pos() && line(cg.End()) == line(kv.Pos())-1:
				e.doc = cg
			case cg.Pos() > kv.Pos() && line(cg.Pos()) == line(kv.Pos()) && line(cg.Pos()) > line(prevEnd):
				e.comment = strings.TrimSpace(cg.Text())
			}
		}
		out = append(out, e)
		prevEnd = kv.End()
	}
	return out, nil
}

// constString evaluates a map key: a string literal, or a string constant
// of this package or an imported one.
func (g *gen) constString(p *pkg, f *ast.File, e ast.Expr) (string, error) {
	switch t := e.(type) {
	case *ast.BasicLit:
		if t.Kind == token.STRING {
			return strconv.Unquote(t.Value)
		}
	case *ast.Ident:
		if v, ok := p.strConsts[t.Name]; ok {
			return v, nil
		}
		return "", fmt.Errorf("%s is not a string constant", t.Name)
	case *ast.SelectorExpr:
		x, ok := t.X.(*ast.Ident)
		if !ok {
			break
		}
		path, err := g.importPath(f, x.Name)
		if err != nil {
			return "", err
		}
		tp, err := g.load(path)
		if err != nil {
			return "", err
		}
		if v, ok := tp.strConsts[t.Sel.Name]; ok {
			return v, nil
		}
		return "", fmt.Errorf("%s.%s is not a string constant", x.Name, t.Sel.Name)
	}
	return "", fmt.Errorf("unsupported key %T", e)
}
