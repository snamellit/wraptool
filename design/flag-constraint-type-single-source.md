# Design: single source of truth for flag-constraint types

Status: **proposal**
Scope: make `config.FlagConstraint.Type` a single, authoritative definition
that both the MCP-schema (`mcptools`) and policy-enforcement (`whitelist`)
layers derive from, and make an illegal constraint type unrepresentable in a
loaded config. No behaviour change to valid configs; the goal is to close a
seam where the two consumers silently drifted.

## 1. Problem

`FlagConstraint.Type` is a `string` enum declared in `internal/config`, but its
legal set and meaning are re-interpreted independently in at least three places,
with no shared owner:

| Site | Knows about `Type` | Unknown-type behaviour |
|---|---|---|
| `config.Validate` | nothing — it does not check `Type` at all | (loads clean) |
| `mcptools.FlagSchemaType` / `IsBooleanFlag` | `integer`, `boolean`, `string` | **silently defaults to `string`** |
| `whitelist.validateValue` | `integer`, `""` | **hard error** ("unknown constraint type") |

The set of valid types lives nowhere in particular; each consumer hard-codes its
own string literals in its own `switch`. Go's type system cannot catch the drift
because it is a bare `string`, not an enum with exhaustiveness checking.

The clinching tell: the two consumers do not even agree on the *unknown* case —
`mcptools` degrades an unrecognised type to a string param, `whitelist` rejects
it. When two modules handle the same unknown input with opposite policies, there
is definitively no single authority for the enum.

## 2. Evidence — the `type: boolean` bug

Fixed reactively in **MR !7 / commit `bdef70e`**. A `flag_constraints` entry of
`type: boolean` (needed so a valueless flag like `glab mr view --comments`
renders as a bare `--comments`) produced the worst possible failure shape:

1. Config **loads clean** — `Validate` never looked at the type.
2. Tool **registers fine** — `mcptools` renders a bare boolean flag.
3. Every **call is denied at runtime** — `whitelist.validateValue` hit its
   `default` branch and returned `unknown constraint type "boolean"`.
4. **All unit tests green** — each module was correct in isolation; nothing
   owned or tested the seam between them.

`--yes` on `glab mr create` was latently broken the same way, only masked
because that flag happened never to be exercised.

MR !7 patched `whitelist` to accept `boolean`/`string`, but that is a band-aid:
it re-synchronised two hand-rolled switches by hand. The next type added to one
and not the other will drift again.

## 3. Goal

- **Single source of truth**: one definition of the legal constraint types and
  their semantics, consumed by config validation, `mcptools` rendering, and
  `whitelist` enforcement. Adding a type is one edit.
- **Illegal states unrepresentable**: a `FlagConstraint` carrying an unhandled
  type cannot exist in a loaded config — it fails at decode, not at call time.
- No change to the behaviour of any currently-valid config.

## 4. Proposed design

### 4.1 A typed enum, owned by `config`

`config` is the leaf package (both `mcptools` and `whitelist` already import it;
it must not import them — cf. the existing `flagParamName` duplication in
`config/validate.go` that exists precisely to avoid a `config → mcptools`
cycle). So the authoritative definition belongs in `config`.

```go
// internal/config
type ConstraintType string

const (
    ConstraintUntyped ConstraintType = ""        // no value typing; pattern-only
    ConstraintString  ConstraintType = "string"  // any value; pattern still applies
    ConstraintInteger ConstraintType = "integer" // parsed + range-checked
    ConstraintBoolean ConstraintType = "boolean" // valueless; rendered as a bare flag
)
```

Change `FlagConstraint.Type` from `string` to `ConstraintType` (a string alias,
so YAML still decodes and existing literals mostly keep working).

### 4.2 Make illegal states unrepresentable — reject at decode

Give `ConstraintType` a YAML unmarshaler that rejects anything outside the set,
so a bad type fails when the config is *parsed*, before any consumer sees it:

```go
func (t *ConstraintType) UnmarshalYAML(n *yaml.Node) error {
    switch ConstraintType(n.Value) {
    case ConstraintUntyped, ConstraintString, ConstraintInteger, ConstraintBoolean:
        *t = ConstraintType(n.Value)
        return nil
    default:
        return fmt.Errorf("unknown flag constraint type %q (want string, integer, or boolean)", n.Value)
    }
}
```

Consequence: **both** `Load` and `LoadStrict` reject `type: bogus` (it is an
unmarshal error, not an unknown-field error). This is a deliberate, small change
to the lenient loader — an *invalid value* is not the same as an *unknown key*,
and failing fast on it is the whole point of "unrepresentable." Downstream code
can then switch on the finite set with **no `default`/unknown branch to
diverge** — the failure mode that started this simply cannot occur.

### 4.3 One semantics table, two consumers

Capture each type's two axes — how it is *presented* to the AI and how its value
is *enforced* — in one table in `config`, and have both layers read from it
instead of re-deriving from literals:

```go
type ConstraintKind struct {
    Valueless  bool   // boolean → rendered as a bare --flag, nothing to validate
    JSONSchema string // "string" | "integer" | "boolean" — for the MCP tool schema
}
var constraintKinds = map[ConstraintType]ConstraintKind{ ... }
```

- `mcptools` derives the JSON-schema type and boolean-ness from this table
  (retiring the literal switches in `FlagSchemaType` / `IsBooleanFlag`; the
  name-heuristic in `IsBooleanFlag` stays, it is a separate concern).
- `whitelist.validateValue` derives "is there a value to check" from the same
  table: `integer` → parse + range; everything else → pattern-only; `boolean`
  (valueless) → accept presence. No `default` error path.

**Concurrency — the table is immutable after init.** `constraintKinds` is read
concurrently: policy evaluation runs on many in-flight MCP handler goroutines,
and tool registration re-runs on reload. A Go map is safe for concurrent reads
*only* if nothing writes to it concurrently, so the table must be populated once
at package init and **never mutated afterwards**. Keep it unexported and expose
lookups through a value-returning helper (`func kindOf(t ConstraintType)
(ConstraintKind, bool)`) rather than handing out the map, so no caller can
accidentally write to it — treat it as an effectively-`const` lookup table.
(Raised in MR !8 review.)

### 4.4 Note: `Type` conflates presentation and validation

`boolean` and `string` are *presentation-only* — they tell `mcptools` how to
render the flag but carry no value-validation semantics; only `integer` does
both. That conflation is why `whitelist` was forced to have an opinion about
types it fundamentally does not care about, and got it wrong. The table in §4.3
keeps the two axes explicit so a presentation-only type never again implies a
validation rule that a consumer must guess at.

### 4.5 Hot-reload: the table is code, the config is data

The §4.3 table is immutable at init, yet wraptool hot-reloads config — those do
not conflict, because they are different layers:

- **`constraintKinds` is the *vocabulary* of the type system** — what `boolean`
  / `integer` / `string` *mean*. That is a property of wraptool's **code**, set
  at build time. Editing a `config.yaml` never changes what `boolean` means.
- **A `config.yaml` is the *program*** — *which* constraints exist and *what
  values* they carry. That is the mutable data hot-reload swaps.

The table is the **interpreter**; the config is the **program**. Reloading a new
program does not require mutating the interpreter. Concretely, on reload:
`config.Load` re-parses the file into new `FlagConstraint` values, `policy.Reload`
swaps that config data in under its lock (as today), and a subsequent call has
`whitelist` look up `kindOf(constraint.Type)` in the **unchanged** table.
`kindOf(boolean)` returns the same answer before and after the reload, so the
table is never written to — the mutation all lives in the swapped config.

The one thing that genuinely *cannot* hot-reload is a **brand-new constraint
type** (e.g. `type: duration`): that is a code change — a new enum member, table
entry, and consumer handling — so it needs a rebuild + restart, exactly as the
`type: boolean` fix did in MR !7. That is the correct boundary: any config using
the already-compiled vocabulary hot-reloads freely; a *new* vocabulary cannot be
loaded into a running binary. Immutability-at-init therefore does not narrow
what operators can reload. (Raised in MR !8 review.)

## 5. Phased plan

1. **Introduce the enum + unmarshaler** (§4.1–4.2) in `config`; switch
   `FlagConstraint.Type` to `ConstraintType`. Update literals across the tree.
2. **Route both consumers through the shared table** (§4.3): retire the string
   switches in `mcptools` and `whitelist`; both read `constraintKinds`.
3. **Add a seam/contract test** — for every `ConstraintType`, assert it is both
   rendered by `mcptools` *and* accepted by `whitelist`. This is the test no
   module owned; it must live where it can import both (e.g. a small
   integration test package) so the seam can never silently break again.
4. **Reconcile `config.Validate`** — type-checking is now handled at decode, so
   `Validate` no longer needs a type check; keep it only if a belt-and-suspenders
   assertion is wanted.
5. **Extend `wraptool config validate`** — with §4.2 it already catches bad
   types (decode fails), so the command gains this for free; add a test proving
   `type: bogus` is reported.

## 6. Testing

- Round-trip contract test per type (§5.3) — the load-bearing addition.
- `LoadStrict`/`Load` both reject `type: bogus` with a clear message.
- Existing `whitelist` constraint tests and `mcptools` typing tests keep passing
  unchanged (valid-config behaviour is preserved).

## 7. Risks / alternatives

- **Behaviour change**: the lenient `Load` starts rejecting invalid constraint
  *values* (not just enforcement-time). This is intended and narrow — it only
  affects configs that were already broken (their constraint silently did
  nothing or got denied per-call). Worth calling out in the changelog.
- **Churn**: flipping `Type`'s underlying type touches every `"boolean"`-style
  literal. Mechanical, compiler-guided.
- **Alternative — validate-only (no enum)**: just have `config.Validate` reject
  unknown types and keep the string switches in sync manually. Simpler diff, but
  it does **not** make illegal states unrepresentable (call sites still carry a
  `default`), and it re-creates the drift risk the moment someone adds a type to
  one switch and not the other. Rejected in favour of the enum + table.
- **Import cycle**: avoided by keeping the definition and table in `config` (the
  leaf) and consuming upward; do not move it into `whitelist`/`mcptools`.
