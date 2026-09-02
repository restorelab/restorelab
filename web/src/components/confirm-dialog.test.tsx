import { render, screen } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { describe, expect, it, vi } from "vitest"
import { ConfirmDialog } from "./confirm-dialog"

describe("ConfirmDialog", () => {
  it("names what it is about to do", () => {
    render(
      <ConfirmDialog
        open
        onOpenChange={() => {}}
        title="Destroy 9001?"
        description="9001 restorelab-110 on pve1, left by run 8f1c6d20."
        confirmLabel="Destroy"
        onConfirm={() => {}}
      />,
    )
    expect(screen.getByText(/9001 restorelab-110 on pve1/)).toBeInTheDocument()
  })

  it("calls back only when the confirming button is pressed", async () => {
    const onConfirm = vi.fn()
    render(
      <ConfirmDialog
        open
        onOpenChange={() => {}}
        title="Destroy 9001?"
        description="It cannot be undone."
        confirmLabel="Destroy"
        onConfirm={onConfirm}
      />,
    )

    await userEvent.click(screen.getByRole("button", { name: /^cancel$/i }))
    expect(onConfirm).not.toHaveBeenCalled()

    await userEvent.click(screen.getByRole("button", { name: /^destroy$/i }))
    expect(onConfirm).toHaveBeenCalledOnce()
  })

  // A dialog that keeps its button live while the request is in flight is a
  // dialog that sends the request twice.
  it("cannot be confirmed twice while it is working", () => {
    render(
      <ConfirmDialog
        open
        onOpenChange={() => {}}
        title="Destroy 9001?"
        description="It cannot be undone."
        confirmLabel="Destroy"
        pending
        onConfirm={() => {}}
      />,
    )
    expect(screen.getByRole("button", { name: /working/i })).toBeDisabled()
  })

  // The refusal has to land in the dialog: a box that closes on an error the
  // viewer never saw is a box that lies about having worked.
  it("shows the refusal without closing", () => {
    render(
      <ConfirmDialog
        open
        onOpenChange={() => {}}
        title="Destroy 9001?"
        description="It cannot be undone."
        confirmLabel="Destroy"
        error="This workload is not one of ours"
        onConfirm={() => {}}
      />,
    )
    expect(screen.getByText(/not one of ours/i)).toBeInTheDocument()
    expect(screen.getByText("Destroy 9001?")).toBeInTheDocument()
  })

  it("renders nothing while closed", () => {
    render(
      <ConfirmDialog
        open={false}
        onOpenChange={() => {}}
        title="Destroy 9001?"
        description="It cannot be undone."
        confirmLabel="Destroy"
        onConfirm={() => {}}
      />,
    )
    expect(screen.queryByText("Destroy 9001?")).toBeNull()
  })
})
