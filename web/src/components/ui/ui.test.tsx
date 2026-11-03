import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { act } from "react";
import { ToastProvider, useToast } from "../Toasts";
import {
  Button,
  Checkbox,
  Tooltip,
  ConfirmDialog,
  CopyField,
  ErrorState,
  Field,
  Input,
  Meter,
  Segmented,
  SignalRibbon,
  StatusIndicator,
} from ".";
import Gallery from "./Gallery";

describe("Button", () => {
  it("defaults to type=button so it cannot submit a form by accident", () => {
    render(<Button>Save</Button>);
    expect(screen.getByRole("button", { name: "Save" })).toHaveAttribute("type", "button");
  });

  it("blocks clicks and reports busy while running", async () => {
    const onClick = vi.fn();
    render(
      <Button busy onClick={onClick}>
        Saving…
      </Button>,
    );
    const button = screen.getByRole("button", { name: "Saving…" });
    expect(button).toHaveAttribute("aria-busy", "true");
    await userEvent.click(button);
    expect(onClick).not.toHaveBeenCalled();
  });
});

describe("Field", () => {
  it("ties the label and the hint to the control", () => {
    render(
      <Field label="Name" hint="Shown in alerts.">
        {(c) => <Input {...c} />}
      </Field>,
    );
    expect(screen.getByLabelText("Name")).toHaveAccessibleDescription("Shown in alerts.");
    expect(screen.getByLabelText("Name")).not.toHaveAttribute("aria-invalid");
  });

  it("replaces the hint with the error and marks the control invalid", () => {
    render(
      <Field label="Name" hint="Shown in alerts." error="Enter a name.">
        {(c) => <Input {...c} />}
      </Field>,
    );
    const input = screen.getByLabelText("Name");
    expect(input).toHaveAttribute("aria-invalid", "true");
    expect(input).toHaveAccessibleDescription("Enter a name.");
    expect(screen.queryByText("Shown in alerts.")).not.toBeInTheDocument();
  });

  it("keeps the label association when the input has a fixed prefix", () => {
    render(<Field label="URL">{(c) => <Input {...c} start="https://" />}</Field>);
    expect(screen.getByLabelText("URL").tagName).toBe("INPUT");
  });
});

describe("Segmented", () => {
  function Example() {
    const [value, setValue] = useState<"24h" | "7d">("24h");
    return (
      <Segmented
        label="Time range"
        name="range"
        value={value}
        onChange={setValue}
        options={[
          { value: "24h", label: "24h" },
          { value: "7d", label: "7d" },
        ]}
      />
    );
  }

  it("is a named radio group that changes on click", async () => {
    render(<Example />);
    expect(screen.getByRole("radiogroup", { name: "Time range" })).toBeInTheDocument();
    await userEvent.click(screen.getByRole("radio", { name: "7d" }));
    expect(screen.getByRole("radio", { name: "7d" })).toBeChecked();
    expect(screen.getByRole("radio", { name: "24h" })).not.toBeChecked();
  });
});

describe("StatusIndicator", () => {
  it("always carries the word, so status is never colour alone", () => {
    render(
      <>
        <StatusIndicator status="up" />
        <StatusIndicator status="pending" />
        <StatusIndicator status="down" label="Down 12 min" />
      </>,
    );
    expect(screen.getByText("Up")).toBeInTheDocument();
    expect(screen.getByText("Pending")).toBeInTheDocument();
    expect(screen.getByText("Down 12 min")).toBeInTheDocument();
  });
});

describe("Meter", () => {
  it("stays quiet below 80 %", () => {
    render(<Meter label="Monitors" used={1} limit={3} />);
    expect(screen.getByRole("meter", { name: "Monitors" })).toHaveAttribute("aria-valuenow", "1");
    expect(screen.queryByText(/left|Limit reached/)).not.toBeInTheDocument();
  });

  it("says how much is left near the limit and when it is reached", () => {
    const { rerender } = render(<Meter label="Monitors" used={4} limit={5} />);
    expect(screen.getByText("1 left")).toBeInTheDocument();
    rerender(<Meter label="Monitors" used={5} limit={5} />);
    expect(screen.getByText("Limit reached")).toBeInTheDocument();
  });
});

describe("ErrorState", () => {
  it("alerts and retries in both variants", async () => {
    const onRetry = vi.fn();
    const { rerender } = render(<ErrorState title="Could not load" onRetry={onRetry} />);
    expect(screen.getByRole("alert")).toHaveTextContent("Could not load");
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    rerender(<ErrorState variant="section" title="Could not load" onRetry={onRetry} />);
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(onRetry).toHaveBeenCalledTimes(2);
  });
});

describe("CopyField", () => {
  const stubClipboard = (writeText: (text: string) => Promise<void>) =>
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });

  it("copies and confirms", async () => {
    render(<CopyField label="Heartbeat URL" value="https://x/1" />);
    const writeText = vi.fn(() => Promise.resolve());
    stubClipboard(writeText);
    fireEvent.click(screen.getByRole("button", { name: "Copy" }));
    await screen.findByRole("button", { name: "Copied ✓" });
    expect(writeText).toHaveBeenCalledWith("https://x/1");
    expect(screen.getByLabelText("Heartbeat URL")).toHaveValue("https://x/1");
  });

  it("never says Copied when the clipboard refused", async () => {
    render(<CopyField label="Heartbeat URL" value="https://x/1" />);
    stubClipboard(() => Promise.reject(new Error("denied")));
    fireEvent.click(screen.getByRole("button", { name: "Copy" }));
    await waitFor(() => expect(screen.getByRole("alert")).toHaveTextContent("Copying failed"));
    expect(screen.queryByRole("button", { name: "Copied ✓" })).not.toBeInTheDocument();
  });
});

describe("ConfirmDialog", () => {
  const setup = (busy = false) => {
    const onConfirm = vi.fn();
    const onCancel = vi.fn();
    render(
      <ConfirmDialog
        title="Delete Checkout API?"
        confirmLabel="Delete monitor"
        busy={busy}
        onConfirm={onConfirm}
        onCancel={onCancel}
      >
        <p>Consequences.</p>
      </ConfirmDialog>,
    );
    return { onConfirm, onCancel };
  };

  it("opens named and described, with focus on Cancel", () => {
    setup();
    const dialog = screen.getByRole("dialog", { name: "Delete Checkout API?" });
    expect(dialog).toHaveAccessibleDescription("Consequences.");
    expect(screen.getByRole("button", { name: "Cancel" })).toHaveFocus();
  });

  it("confirms and cancels through its buttons", async () => {
    const { onConfirm, onCancel } = setup();
    await userEvent.click(screen.getByRole("button", { name: "Delete monitor" }));
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onConfirm).toHaveBeenCalledOnce();
    expect(onCancel).toHaveBeenCalledOnce();
  });

  it("treats the native cancel (Escape) as Cancel, unless the action is running", () => {
    const { onCancel } = setup();
    fireEvent(screen.getByRole("dialog"), new Event("cancel", { cancelable: true }));
    expect(onCancel).toHaveBeenCalledOnce();
  });

  it("cancels on Escape", async () => {
    const { onCancel } = setup();
    await userEvent.keyboard("{Escape}");
    expect(onCancel).toHaveBeenCalledOnce();
  });

  it("cannot be cancelled while busy", () => {
    const { onCancel } = setup(true);
    fireEvent(screen.getByRole("dialog"), new Event("cancel", { cancelable: true }));
    expect(onCancel).not.toHaveBeenCalled();
  });
});

describe("SignalRibbon", () => {
  const points = [
    { t: "2026-11-02T03:00:00Z", avgMs: 120, okRatio: 1 },
    { t: "2026-11-02T03:15:00Z", avgMs: null, okRatio: null },
    { t: "2026-11-02T03:30:00Z", avgMs: null, okRatio: 0 },
  ];

  it("is hidden from assistive technology when it has no label", () => {
    const { container } = render(<SignalRibbon points={points} heightPx={18} />);
    expect(container.querySelector('[aria-hidden="true"]')).toBeInTheDocument();
    expect(screen.queryByRole("group")).not.toBeInTheDocument();
  });

  it("can be inspected with the keyboard, starting at the newest bucket", async () => {
    render(<SignalRibbon points={points} label="Checkout API" />);
    const ribbon = screen.getByRole("group", { name: /Checkout API/ });
    await userEvent.tab();
    expect(ribbon).toHaveFocus();
    expect(screen.getByRole("status")).toHaveTextContent("uptime 0% · no response");
    await userEvent.keyboard("{ArrowLeft}");
    expect(screen.getByRole("status")).toHaveTextContent("No checks ran");
    await userEvent.keyboard("{Home}");
    expect(screen.getByRole("status")).toHaveTextContent("uptime 100% · avg 120 ms");
    await userEvent.tab();
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });
});

describe("Gallery", () => {
  it("renders every section without crashing", () => {
    render(<Gallery />);
    expect(screen.getByRole("heading", { name: "UI kit" })).toBeInTheDocument();
    expect(screen.getByText(/07 · Signal ribbon/)).toBeInTheDocument();
  });
});

describe("Checkbox", () => {
  it("describes the box with its error and its description", () => {
    render(
      <Checkbox
        label="Notify Slack"
        description="Posts on changes."
        error="Add a webhook first."
      />,
    );
    const box = screen.getByLabelText("Notify Slack");
    expect(box).toHaveAttribute("aria-invalid", "true");
    expect(box).toHaveAccessibleDescription("Add a webhook first. Posts on changes.");
  });
});

describe("Tooltip", () => {
  it("adds to the child's own description", () => {
    render(
      <>
        <span id="own">Your plan allows 3.</span>
        <Tooltip content="More soon.">
          <button type="button" aria-describedby="own">
            Add
          </button>
        </Tooltip>
      </>,
    );
    expect(screen.getByRole("button", { name: "Add" })).toHaveAccessibleDescription(
      "Your plan allows 3. More soon.",
    );
  });
});

describe("ToastProvider", () => {
  function Saver() {
    const toast = useToast();
    return (
      <button type="button" onClick={() => toast("Saved")}>
        Save
      </button>
    );
  }

  afterEach(() => vi.useRealTimers());

  it("keeps success toasts while the pointer is over them, also new ones", () => {
    vi.useFakeTimers();
    render(
      <ToastProvider>
        <Saver />
      </ToastProvider>,
    );
    const region = screen.getAllByRole("status").find((e) => e.getAttribute("aria-live"))!;
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    fireEvent.mouseEnter(region);
    fireEvent.click(screen.getByRole("button", { name: "Save" }));
    act(() => vi.advanceTimersByTime(10_000));
    expect(screen.getAllByText("Saved")).toHaveLength(2);

    fireEvent.mouseLeave(region);
    act(() => vi.advanceTimersByTime(4_000));
    expect(screen.getAllByText("Saved")).toHaveLength(2);
    act(() => vi.advanceTimersByTime(1_500));
    expect(screen.queryByText("Saved")).not.toBeInTheDocument();
  });
});
