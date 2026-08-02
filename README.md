# vectorcore-GMLC
Gateway Mobile Location Centre

### work in progress ....

## TODO

- Add an OMA Mobile Location Protocol (MLP) adapter for the Le interface
  (LCS Client ↔ GMLC). The current REST/JSON API (`internal/httpapi`) is a
  non-standard interim adapter kept for testing; MLP is the actual
  standards-based Le protocol (SLIR/SLIA at minimum, to match the immediate
  location support already implemented). It should sit alongside the JSON
  API as a second adapter over the existing protocol-neutral `service.Service`
  core, not replace it. See `docs/architecture.md` for the adapter layering.
