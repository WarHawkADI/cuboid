# Judges' path needs only Docker; these targets are conveniences around it.
# Windows without make: go run ./tools/task <target>. The two share one list.
TARGETS := up down seed seed-main keys test verify demo-check speed-test freshness-test safety-test \
	correctness-test growth-test targets bench chaos drill isolation isolation-full rebuild web contrast

.PHONY: $(TARGETS)
$(TARGETS):
	go run ./tools/task $@
