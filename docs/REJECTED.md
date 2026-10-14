# Rejected Designs

Ideas that were **decided against**, not merely deferred. Recording them prevents
a "designed but not built" feature from implying a capability that does not
exist. A rejected entry must state what it was, concrete reasons (not
"unimplemented"), where it is excluded, and — only if a cheap honest path exists
— a clearly-labelled *reconsidered* note that proposes but does not implement.

## Image pixel-surface pyramid — REJECTED

**What it was.** An earlier reading of the originating research proposed a set of
decoded, downsampled image surfaces materialised behind the namespace (serving a
thumbnail-like level), i.e. `pyramid_levels(width, height)` reporting conventional
halving geometry.

**Why rejected.**

1. **Geometry alone provides no user-visible capability.** Parsing a format
   header's dimensions and reporting conventional halving geometry is a
   *descriptive* answer about the header; it accelerates no query and nothing
   reads it back as an image. Keeping it "designed but not materialised" would
   imply a capability that does not exist.
2. **Materialising real surfaces needs a decoder dependency the project
   deliberately avoids.** Producing PNG surfaces needs zlib/DEFLATE inflate, PNG
   filter reconstruction, interlace handling, and colour-space conversion.
   OrFS-GO is a minimal-dependency build; hand-rolling an unaudited inflater
   would threaten pixel/colour correctness.
3. **Not on any critical path.** No query, read path, recovery, or POSIX op
   depends on a pixel surface, and no court would measure one.

**Where it is excluded.** This build implements no image parsing and no pixel
surfaces at all; `internal/` contains no image package, and nothing in the
namespace or manifestation plane references one.

**Reconsidered (proposal only, NOT implemented).** A dependency-free
uncompressed-BMP (24/32-bit `BI_RGB`) path — little-endian, row-padded, no
compression — would need no inflate and could be added as a separate task with
its own court. Until then the pyramid remains rejected.

**Rule for adding a rejected candidate.** State what it was, concrete reasons,
where it is excluded, and, only if a cheap honest path exists, a clearly-labelled
*reconsidered* note that proposes rather than implements.
