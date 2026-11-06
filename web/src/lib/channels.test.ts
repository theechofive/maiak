import {
  emptyChannelForm,
  eventsLabel,
  reminderLabel,
  toChannelCreate,
  toChannelUpdate,
  validateChannelForm,
  type ChannelFormValues,
} from "./channels";

const valid: ChannelFormValues = {
  ...emptyChannelForm,
  name: " #ops ",
  webhookUrl: " https://hooks.slack.com/services/T0/B0/x ",
};

describe("channel form", () => {
  it("accepts a valid form and builds the bodies", () => {
    expect(validateChannelForm(valid, true)).toEqual({});
    expect(
      toChannelCreate({ ...valid, mention: " @here  <@U0ABC> @here", reminderMinutes: "15" }),
    ).toEqual({
      name: "#ops",
      webhookUrl: "https://hooks.slack.com/services/T0/B0/x",
      events: ["down", "recovered", "tls_expiring"],
      mention: ["@here", "<@U0ABC>"],
      reminderMinutes: 15,
    });
    // Editing without a new URL keeps the stored webhook.
    expect(toChannelUpdate({ ...valid, webhookUrl: "" })).not.toHaveProperty("webhookUrl");
    expect(validateChannelForm({ ...valid, webhookUrl: "" }, false)).toEqual({});
    expect(validateChannelForm({ ...valid, webhookUrl: "" }, true)).toHaveProperty("webhookUrl");
  });

  it("rejects what the server would reject", () => {
    const errors = validateChannelForm(
      {
        name: "",
        project: "",
        webhookUrl: "https://example.com/hook",
        events: [],
        mention: "@everyone",
        reminderMinutes: "4",
      },
      true,
    );
    expect(Object.keys(errors).sort()).toEqual([
      "events",
      "mention",
      "name",
      "reminderMinutes",
      "webhookUrl",
    ]);
    expect(
      validateChannelForm({ ...valid, mention: "@here @channel <@U1AB> <@U2AB>" }, true),
    ).toHaveProperty("mention");
    expect(validateChannelForm({ ...valid, mention: "<https://evil|x>" }, true)).toHaveProperty(
      "mention",
    );
    expect(validateChannelForm({ ...valid, reminderMinutes: "1441" }, true)).toHaveProperty(
      "reminderMinutes",
    );
    expect(validateChannelForm({ ...valid, reminderMinutes: "" }, true)).toEqual({});
    expect(validateChannelForm({ ...valid, name: "x".repeat(61) }, true)).toHaveProperty("name");
  });

  it("labels events and reminders", () => {
    expect(eventsLabel(["tls_expiring", "down"])).toBe("Down, TLS certificate expiring");
    expect(reminderLabel(0)).toBe("Off");
    expect(reminderLabel(30)).toBe("Every 30 min");
    expect(reminderLabel(120)).toBe("Every 2 h");
  });
});
