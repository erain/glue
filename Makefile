.PHONY: ax-dev-prereqs ax-dev-env ax-dev-up ax-dev-task-image ax-dev-gateway ax-dev-status ax-dev-ready ax-dev-smoke ax-dev-manual-job ax-dev-e2e-submit ax-dev-e2e-suspend ax-dev-down
ax-dev-prereqs:     ; ./scripts/ax-dev/prereqs.sh
ax-dev-env:         ; ./scripts/ax-dev/env.sh
ax-dev-up:          ; ./scripts/ax-dev/up.sh
ax-dev-task-image:  ; ./scripts/ax-dev/build-task-image.sh
ax-dev-gateway:     ; ./scripts/ax-dev/deploy-gateway.sh
ax-dev-status:      ; ./scripts/ax-dev/status.sh
ax-dev-ready:       ; ./scripts/ax-dev/status.sh --quiet
ax-dev-smoke:       ; ./scripts/ax-dev/smoke.sh
ax-dev-manual-job:  ; ./scripts/ax-dev/manual-job.sh $(ARGS)
ax-dev-e2e-submit:  ; ./scripts/ax-dev/e2e/submit.sh $(ARGS)
ax-dev-e2e-suspend: ; ./scripts/ax-dev/e2e/suspend-resume.sh $(ARGS)
ax-dev-down:        ; ./scripts/ax-dev/down.sh
