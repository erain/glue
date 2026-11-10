.PHONY: ax-dev-prereqs ax-dev-up ax-dev-task-image ax-dev-status ax-dev-smoke ax-dev-manual-job ax-dev-down
ax-dev-prereqs:    ; ./scripts/ax-dev/prereqs.sh
ax-dev-up:         ; ./scripts/ax-dev/up.sh
ax-dev-task-image: ; ./scripts/ax-dev/build-task-image.sh
ax-dev-status:     ; ./scripts/ax-dev/status.sh
ax-dev-smoke:      ; ./scripts/ax-dev/smoke.sh
ax-dev-manual-job: ; ./scripts/ax-dev/manual-job.sh $(ARGS)
ax-dev-down:       ; ./scripts/ax-dev/down.sh
