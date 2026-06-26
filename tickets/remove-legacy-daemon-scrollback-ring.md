# Remove legacy daemon scrollback ring

<!-- openkanban:card-notes start -->
## Notes (from openkanban card)

After PR #142 (daemon snapshot reads native scrollback), the daemon-side ScrollbackBuffer ring + CaptureTopRow/PushScrolledLine + Pane.View/RenderVT (internal/terminal) have ZERO production readers. The ring is still populated by handleOutput but only the dead Pane.View path reads it. SHAPE: delete the ring field + capture helpers + Pane.View/RenderVT/scrollUp-Down ring paths; update TestAttach_SnapshotIncludesScrollback's precondition (currently reads the ring via ScrollbackLen) to assert native scrollback instead. WHY: removes a per-write CaptureTopRow cost on the handleOutput hot path and a confusing dead parallel source. RE-EVALUATE WHEN: doing any other internal/terminal/pane.go work, or if anything starts reading the daemon Pane's own rendered View. LOW priority housekeeping. Resolves nothing live; companion to f52cb988 (fixed by PR #142).
<!-- openkanban:card-notes end -->
