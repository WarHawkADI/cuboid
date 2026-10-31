# Judges' path needs only Docker; these targets are conveniences around it.
# Windows without make: go run ./tools/task <target>. The two share one list.
TARGETS := setup up down seed seed-main demo keys test verify verify-full clean-clone-check docker-clean-clone-check \
	demo-check speed-test freshness-test safety-test correctness-test growth-test targets scorecard report bench \
	chaos drill redteam isolation isolation-full rebuild web contrast disclosure critique

.PHONY: $(TARGETS)
$(TARGETS):
	go run ./tools/task $@
