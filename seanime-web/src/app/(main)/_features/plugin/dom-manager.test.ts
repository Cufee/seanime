import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

const hooks = vi.hoisted(() => ({
    mainTab: { current: true },
    effects: [] as Array<() => void | (() => void)>,
    sendPluginMessage: vi.fn(),
}))

vi.mock("react", () => ({
    useRef: <T>(current: T) => ({ current }),
    useEffect: (effect: () => void | (() => void)) => hooks.effects.push(effect),
}))
vi.mock("@/app/(main)/_hooks/handle-websockets", () => ({
    useWebsocketSender: () => ({ sendPluginMessage: hooks.sendPluginMessage }),
}))
vi.mock("@/app/websocket-provider", () => ({
    useIsMainTab: () => hooks.mainTab.current,
    useIsMainTabRef: () => hooks.mainTab,
}))
vi.mock("@/lib/helpers/debug", () => ({ logger: () => ({ info: vi.fn() }) }))
vi.mock("./generated/plugin-events", () => ({ PluginClientEvents: {} }))

import { useDOMManager } from "./dom-manager"

// Only the DOM operations used to acquire and release plugin resources are needed here.
class TestElement extends EventTarget {
    id = ""
    attributes: Array<{ name: string; value: string }> = []
    dataset = {}
    style = {}
    children: TestElement[] = []
    parent: TestElement | null = null
    removeEventListener = vi.fn(super.removeEventListener.bind(this))

    constructor(public tagName: string) { super() }

    appendChild(child: TestElement) {
        child.remove()
        child.parent = this
        this.children.push(child)
    }

    remove() {
        if (this.parent) this.parent.children = this.parent.children.filter(child => child !== this)
        this.parent = null
    }

    hasChildNodes() { return this.children.length > 0 }

    querySelectorAll(selector: string): TestElement[] {
        return this.children.flatMap(child => [
            ...(selector === "*" || selector === `#${child.id}` ? [child] : []),
            ...child.querySelectorAll(selector),
        ])
    }
}

class TestObserver {
    static instances: TestObserver[] = []
    observe = vi.fn()
    disconnect = vi.fn()
    constructor() { TestObserver.instances.push(this) }
}

let body: TestElement

function mountManager(extensionId: string) {
    hooks.effects = []
    const manager = useDOMManager(extensionId)
    const cleanups = hooks.effects.map(effect => effect())
    return { ...manager, unmount: () => cleanups.forEach(cleanup => cleanup?.()) }
}

function createElement(manager: ReturnType<typeof mountManager>) {
    manager.handleDOMCreate({ tagName: "button", requestId: "create" })
    const container = body.querySelectorAll("#plugin-dom-container")[0]
    return container.children.at(-1)!
}

function listen(manager: ReturnType<typeof mountManager>, element: TestElement) {
    manager.handleDOMManipulate({
        elementId: element.id,
        action: "addEventListener",
        params: { event: "click", listenerId: "click-listener" },
        requestId: "listen",
    })
}

beforeEach(() => {
    hooks.mainTab.current = true
    hooks.sendPluginMessage.mockClear()
    TestObserver.instances = []
    body = new TestElement("body")
    vi.stubGlobal("Element", TestElement)
    vi.stubGlobal("HTMLElement", TestElement)
    vi.stubGlobal("MutationObserver", TestObserver)
    vi.stubGlobal("IntersectionObserver", TestObserver)
    vi.stubGlobal("window", { addEventListener: vi.fn(), removeEventListener: vi.fn() })
    vi.stubGlobal("document", {
        body,
        readyState: "complete",
        createElement: (tagName: string) => new TestElement(tagName),
        getElementById: (id: string) => body.querySelectorAll(`#${id}`)[0] ?? null,
        querySelectorAll: (selector: string) => body.querySelectorAll(selector),
    })
})

afterEach(() => vi.unstubAllGlobals())

describe("plugin DOM cleanup", () => {
    it("releases owned DOM, listeners, and observers after losing the main-tab role", () => {
        const manager = mountManager("uninstalled-plugin")
        const element = createElement(manager)
        listen(manager, element)
        manager.handleDOMObserveInView({ selector: `#${element.id}`, observerId: "visible" })

        // Layout effects update the role ref before the old main tab's effect cleanup runs.
        hooks.mainTab.current = false
        manager.unmount()

        expect(TestObserver.instances).toHaveLength(2)
        expect(TestObserver.instances.every(observer => observer.disconnect.mock.calls.length === 1)).toBe(true)
        expect(element.removeEventListener).toHaveBeenCalledWith("click", expect.any(Function))
        expect(element.parent).toBeNull()
        expect(body.children).toHaveLength(0)
        expect(() => manager.cleanup()).not.toThrow()
    })

    it("cleans the original nodes without removing a replacement or another manager's listener", () => {
        const oldManager = mountManager("old-plugin")
        const original = createElement(oldManager)
        listen(oldManager, original)
        const oldObserver = TestObserver.instances[0]
        original.remove()

        const newManager = mountManager("new-plugin")
        const replacement = createElement(newManager)
        replacement.id = original.id
        listen(newManager, replacement)
        const newObserver = TestObserver.instances[1]

        hooks.mainTab.current = false
        oldManager.unmount()

        expect(oldObserver.disconnect).toHaveBeenCalledOnce()
        expect(original.removeEventListener).toHaveBeenCalledWith("click", expect.any(Function))
        expect(replacement.parent).not.toBeNull()
        expect(replacement.removeEventListener).not.toHaveBeenCalled()
        expect(newObserver.disconnect).not.toHaveBeenCalled()

        newManager.unmount()
        expect(replacement.parent).toBeNull()
        expect(newObserver.disconnect).toHaveBeenCalledOnce()
    })
})
