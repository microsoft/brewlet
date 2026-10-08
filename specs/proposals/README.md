# Brewlet enhancement proposals

KEP-style design proposals for changes that are large enough to warrant a written,
reviewable design. The [roadmap](../../ROADMAP.md) is the source of truth for
functionality that Brewlet does not yet ship; proposals preserve the detailed
engineering rationale behind those roadmap items. These proposals are not current
contracts; see the [specification](../SPECIFICATION.md) for shipped behavior.
Implemented proposals 0001-0004 are preserved as non-normative historical records
in the [archive index](../../archive/README.md).

## Roadmap designs

- [0005 — Replica coalescing: fewer, larger JVMs](0005-replica-coalescing.md)
  — proposed
- [0006 — Sandbox isolation tiers](0006-sandbox-isolation-tiers.md)
  — research complete; implementation gated on prototype
- [0007 — Baked golden-image delivery](0007-baked-golden-image-delivery.md)
  — proposed as an optional delivery mode, not a replacement
- [0008 — Per-application Java truststores](0008-application-truststores.md)
  — proposed Secret/ConfigMap-backed truststore references for `JavaApplication`
- [0009 — Brewlet 2.0: language-agnostic runtime images](0009-language-agnostic-runtime-images.md)
  — draft; breaking redesign replacing the JDK inventory with approved runtime images
