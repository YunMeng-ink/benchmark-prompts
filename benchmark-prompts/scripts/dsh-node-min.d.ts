/**
 * dsh-node-min.d.ts —— 环境缺 @types/node 时的最小 Node 全局类型替身。
 * 仅覆盖 plugins/dsh 源码用到的表面；找到真 @types/node 时类型检查不用本文件。
 */
interface AbortSignal {
  readonly aborted: boolean
  readonly reason: unknown
  addEventListener(type: 'abort', listener: () => void, options?: { once?: boolean }): void
  removeEventListener(type: 'abort', listener: () => void): void
  throwIfAborted(): void
}
declare var AbortSignal: {
  new (): AbortSignal
  abort(reason?: unknown): AbortSignal
  timeout(ms: number): AbortSignal
}
interface AbortController {
  readonly signal: AbortSignal
  abort(reason?: unknown): void
}
declare var AbortController: { new (): AbortController }

declare function setTimeout(handler: () => void, ms?: number): number
declare function clearTimeout(id: number | undefined): void

declare var process: {
  cwd(): string
  env: Record<string, string | undefined>
  platform: string
}

declare namespace NodeJS {
  interface ProcessEnv {
    [key: string]: string | undefined
  }
}
