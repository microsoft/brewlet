// Incremental parser for integration-test output.
//
// Understands three formats:
//   * e2e/run.sh tiers:    "== Tier N — title ==", "  PASS name", "  FAIL name — detail",
//                          "  SKIP name — detail", "-- info", "!! warn".
//   * e2e/live/*.py:       "PASS: name", "Evidence: <dir>", Python tracebacks.
//   * python -m unittest:  "test_x (mod.Class.test_x) ... ok|FAIL|ERROR|skipped '...'".

const ANSI = /\x1b\[[0-9;]*[A-Za-z]/g;
const SECTION = /^== (.+) ==\s*$/;
const RESULT = /^\s*(PASS|FAIL|SKIP)\s+(.*)$/;
const LIVE_PASS = /^PASS: (.+)$/;
const UNITTEST = /^(.*?) \.\.\. (ok|FAIL|ERROR|skipped(?: .*)?|expected failure|unexpected success)\s*$/;
const UNITTEST_PENDING = /^(test\w*) \(([\w.]+)\)/;
const TIER = /^Tier (\d+)([a-z]?)\b/;

export function createParseState() {
    return {
        sections: [],
        results: [],
        warnings: [],
        counts: { pass: 0, fail: 0, skip: 0 },
        tiersSeen: [],
        current: null,
        inSummary: false,
        lastLine: "",
        lastLineAt: null,
        lineCount: 0,
        evidence: [],
        errorTail: [],
        inTraceback: false,
        partial: "",
        unittestRan: null,
        pendingTest: null,
    };
}

function openSection(state, title, at) {
    const prev = state.sections[state.sections.length - 1];
    if (prev && !prev.endedAt) prev.endedAt = at;
    const m = TIER.exec(title);
    const section = {
        index: state.sections.length,
        title,
        tier: m ? Number(m[1]) : null,
        startedAt: at,
        endedAt: null,
        counts: { pass: 0, fail: 0, skip: 0 },
        info: [],
    };
    state.sections.push(section);
    state.current = section;
    if (section.tier != null && !state.tiersSeen.includes(section.tier)) state.tiersSeen.push(section.tier);
    state.inSummary = title === "Summary";
}

function addResult(state, status, name, detail, at) {
    if (state.inSummary) return;
    if (!state.current) openSection(state, "Run", at);
    const key = status.toLowerCase();
    const result = {
        status,
        name,
        detail: detail || "",
        section: state.current.title,
        tier: state.current.tier,
        at,
    };
    const logRef = /\bsee (\/\S+)/.exec(result.detail);
    if (logRef) result.logPath = logRef[1];
    state.results.push(result);
    state.counts[key]++;
    state.current.counts[key]++;
}

function parseLine(state, raw, at) {
    const line = raw.replace(ANSI, "").replace(/\r$/, "");
    state.lineCount++;
    if (line.trim()) {
        state.lastLine = line;
        state.lastLineAt = at;
    }

    let m;
    if ((m = SECTION.exec(line))) return openSection(state, m[1].trim(), at);

    if ((m = LIVE_PASS.exec(line))) return addResult(state, "PASS", m[1], "", at);

    if ((m = RESULT.exec(line))) {
        const [, status, rest] = m;
        const split = rest.split(" — ");
        return addResult(state, status, split[0], split.slice(1).join(" — "), at);
    }

    if ((m = UNITTEST.exec(line))) {
        let name = m[1].trim();
        if (!UNITTEST_PENDING.test(name) && state.pendingTest) name = `${state.pendingTest} ${name}`;
        state.pendingTest = null;
        const outcome = m[2];
        const status = outcome === "ok" || outcome === "expected failure"
            ? "PASS"
            : outcome.startsWith("skipped") ? "SKIP" : "FAIL";
        const detail = outcome === "ok" ? "" : outcome;
        return addResult(state, status, name, detail, at);
    }
    if ((m = UNITTEST_PENDING.exec(line)) && !line.includes(" ... ")) {
        state.pendingTest = line.trim();
        return;
    }
    if ((m = /^Ran (\d+) tests? in/.exec(line))) {
        state.unittestRan = (state.unittestRan || 0) + Number(m[1]);
        return;
    }

    if ((m = /^Evidence: (.+)$/.exec(line))) {
        state.evidence.push(m[1].trim());
        return;
    }

    if (/^-- /.test(line) && state.current) {
        state.current.info.push(line.slice(3));
        if (state.current.info.length > 40) state.current.info.shift();
        return;
    }
    if (/^!! /.test(line)) {
        state.warnings.push({ text: line.slice(3), section: state.current?.title ?? null, at });
        return;
    }

    if (/^Traceback \(most recent call last\):/.test(line)) {
        state.inTraceback = true;
        state.errorTail = [line];
        return;
    }
    if (state.inTraceback) {
        state.errorTail.push(line);
        if (state.errorTail.length > 60) state.errorTail.shift();
        if (/^\S/.test(line) && !/^Traceback/.test(line) && !/^During handling/.test(line)) {
            state.inTraceback = false;
        }
    }
}

export function feed(state, chunk, at = Date.now()) {
    const text = state.partial + chunk;
    const lines = text.split("\n");
    state.partial = lines.pop();
    const appended = [];
    for (const line of lines) {
        parseLine(state, line, at);
        appended.push(line.replace(ANSI, ""));
    }
    return appended;
}

export function flush(state, at = Date.now()) {
    if (!state.partial) return [];
    const line = state.partial;
    state.partial = "";
    parseLine(state, line, at);
    return [line.replace(ANSI, "")];
}

export function closeSections(state, at) {
    const last = state.sections[state.sections.length - 1];
    if (last && !last.endedAt) last.endedAt = at;
}
