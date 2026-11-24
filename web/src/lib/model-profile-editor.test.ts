import { beforeAll, describe, expect, it } from "vitest";
import { initI18n } from "../i18n";
import type { ModelProfile } from "./model-profiles";
import {
  draftToProfileInput,
  isProfileDraftDirty,
  profileDraftProblems,
  profileToDraft,
} from "./model-profile-editor";

beforeAll(async () => {
  await initI18n("en");
});

const baseProfile: ModelProfile = {
  id: "economy",
  name: "Economy",
  description: "Everyday models",
  purposes: {
    assistant: { provider: "launcher.gw", model: "sonnet-4", effort: "medium", permission_mode: "acceptEdits" },
    kb: {},
  },
  launchers: {
    gw: { model: "sonnet-4", small_model: "", effort: "low" },
  },
  tiers: {
    strong: { launcher: "gw", model: "opus-4", effort: "high", permission_mode: "plan", speed: "fast", description: "Hard work" },
  },
  changes: [],
};

describe("model-profile-editor", () => {
  it("round-trips a profile through draft and save input without emitting config_dir or secrets", () => {
    const draft = profileToDraft(baseProfile, ["assistant", "agent.new", "kb", "inbox.triage", "agent.delegate", "translate"]);
    expect(isProfileDraftDirty(draft, profileToDraft(baseProfile))).toBe(false);
    expect(profileDraftProblems(draft, { gw: "launcher" })).toEqual([]);

    const input = draftToProfileInput(draft, 5);
    expect(input).toEqual({
      revision: 5,
      name: "Economy",
      description: "Everyday models",
      purposes: {
        assistant: { provider: "launcher.gw", model: "sonnet-4", effort: "medium", permission_mode: "acceptEdits" },
        kb: {},
      },
      launchers: {
        gw: { model: "sonnet-4", small_model: "", effort: "low" },
      },
      tiers: {
        strong: { launcher: "gw", model: "opus-4", effort: "high", permission_mode: "plan", speed: "fast", description: "Hard work" },
      },
    });
    expect(JSON.stringify(input)).not.toContain("config_dir");
    expect(JSON.stringify(input)).not.toContain("api_key");
  });

  it("omits unchecked purposes and empty launcher entries", () => {
    const draft = profileToDraft(baseProfile);
    draft.purposes.kb.included = false;
    draft.launchers.gw = {};
    const input = draftToProfileInput(draft, 2);
    expect(input.purposes).toEqual({
      assistant: { provider: "launcher.gw", model: "sonnet-4", effort: "medium", permission_mode: "acceptEdits" },
    });
    expect(input.launchers).toBeUndefined();
    expect(isProfileDraftDirty(draft, profileToDraft(baseProfile))).toBe(true);
  });

  it("validates profile name, launcher API restrictions, and tier constraints", () => {
    const draft = profileToDraft(baseProfile);
    draft.name = "   ";
    draft.launchers.api_only = { model: "m-1", small_model: "m-2" };
    draft.tiers.push({
      key: "bad-1",
      name: "Invalid Name",
      description: "line1\nline2",
      candidates: [
        {
          key: "bad-1-c0",
          launcher: "",
          model: "bad model id!",
          effort: "extreme",
          permission_mode: "bogusMode",
          speed: "warp",
        },
      ],
    });
    const problems = profileDraftProblems(draft, { gw: "launcher", api_only: "api" });
    expect(problems.length).toBeGreaterThanOrEqual(7);
    expect(problems.some((p) => p.includes("Profile name"))).toBe(true);
    expect(problems.some((p) => p.includes("api_only"))).toBe(true);
    expect(problems.some((p) => p.includes("Invalid Name"))).toBe(true);
    expect(problems.some((p) => p.includes("bad model id!") && p.includes("#1"))).toBe(true);
    expect(problems.some((p) => p.includes("permission_mode") && p.includes("#1"))).toBe(true);
  });

  it("detects duplicate tier names and preserves untouched legacy tier model IDs", () => {
    const legacy: ModelProfile = {
      id: "legacy",
      name: "Legacy",
      tiers: {
        old: { launcher: "claude", model: "bad!legacy" },
      },
      changes: [],
    };
    const draft = profileToDraft(legacy);
    // Untouched legacy model ID does not block editing unrelated fields (mirrors server/internal/assistant/model_profiles_edit.go).
    expect(profileDraftProblems(draft)).toEqual([]);

    // Adding a duplicate tier name is flagged.
    draft.tiers.push({
      key: "dup",
      name: "old",
      description: "",
      candidates: [
        {
          key: "dup-c0",
          launcher: "claude",
          model: "valid-model",
          effort: "",
          permission_mode: "",
          speed: "",
        },
      ],
    });
    expect(profileDraftProblems(draft).some((p) => p.includes("Duplicate"))).toBe(true);
  });

  it("round-trips multi-candidate tiers with aliases and keeps single-candidate tiers flat", () => {
    const multi: ModelProfile = {
      id: "routing",
      name: "Routing",
      tiers: {
        dev: {
          launcher: "example-a",
          model: "model-a",
          effort: "medium",
          description: "Development work",
          aliases: ["standard"],
          candidates: [
            { launcher: "example-a", model: "model-a", effort: "medium" },
            { launcher: "example-b", model: "model-b", effort: "high", speed: "fast" },
          ],
        },
      },
      changes: [],
    };
    const draft = profileToDraft(multi);
    expect(draft.tiers[0].aliases).toEqual(["standard"]);
    expect(draft.tiers[0].candidates).toHaveLength(2);
    expect(profileDraftProblems(draft)).toEqual([]);

    // Reorder candidates: second becomes primary flat fields.
    const [first, second] = draft.tiers[0].candidates;
    draft.tiers[0].candidates = [second, first];
    const reordered = draftToProfileInput(draft, 4);
    expect(reordered.tiers?.dev).toEqual({
      launcher: "example-b",
      model: "model-b",
      effort: "high",
      speed: "fast",
      description: "Development work",
      aliases: ["standard"],
      candidates: [
        { launcher: "example-b", model: "model-b", effort: "high", speed: "fast" },
        { launcher: "example-a", model: "model-a", effort: "medium" },
      ],
    });

    // Removing down to one candidate omits `candidates` (legacy flat shape).
    draft.tiers[0].candidates = [first];
    const single = draftToProfileInput(draft, 4);
    expect(single.tiers?.dev).toEqual({
      launcher: "example-a",
      model: "model-a",
      effort: "medium",
      description: "Development work",
      aliases: ["standard"],
    });
    expect(single.tiers?.dev?.candidates).toBeUndefined();
  });

  it("validates candidate count limits and includes 1-based candidate indices in errors", () => {
    const draft = profileToDraft(baseProfile);
    draft.tiers[0].candidates.push({
      key: "bad-c2",
      launcher: "",
      model: "not valid!",
      effort: "bogus",
      permission_mode: "",
      speed: "ludicrous",
    });
    const problems = profileDraftProblems(draft);
    expect(problems.some((p) => p.includes("#2") && p.includes("launcher"))).toBe(true);
    expect(problems.some((p) => p.includes("#2") && p.includes("not valid!"))).toBe(true);
    expect(problems.some((p) => p.includes("#2") && p.includes("bogus"))).toBe(true);
    expect(problems.some((p) => p.includes("#2") && p.includes("ludicrous"))).toBe(true);

    // More than 6 candidates is rejected.
    while (draft.tiers[0].candidates.length <= 6) {
      draft.tiers[0].candidates.push({
        key: `extra-${draft.tiers[0].candidates.length}`,
        launcher: "example-a",
        model: "",
        effort: "",
        permission_mode: "",
        speed: "",
      });
    }
    expect(profileDraftProblems(draft).some((p) => p.includes("at most 6"))).toBe(true);
  });
});
