/**
 * dsh-module-hook.mjs —— 把裸的 @deepseek-ai/* 依赖改判到 DSH 安装树
 * （~/.dsh/profiles/node_modules）下解析，让本仓库 plugins/dsh/*.ts 的原位
 * `node --test` / 脚本冒烟看到与装载进 DSH 时同一份框架实例。
 *
 * 动机：DSH 里装载时，裸导入沿目录上溯命中 ~/.dsh/profiles/node_modules；
 * 仓库原位置没有这些包。可用 DSH_PROFILES 环境变量覆盖安装树位置。
 *
 * 用法：node --import scripts/dsh-module-hook.mjs --test plugins/dsh/plugin.test.ts
 */
import { registerHooks } from 'node:module'
import { homedir } from 'node:os'
import { join } from 'node:path'
import { pathToFileURL } from 'node:url'

const profilesDir = process.env.DSH_PROFILES ?? join(homedir(), '.dsh', 'profiles')
// 用安装树目录内一个虚构兄弟文件当 parentURL：上溯从该目录开始，
// @deepseek-ai/*（含其传递依赖 cosmokit/zod 等）即可命中 profiles/node_modules。
const shimParent = new URL('__dsh-hook-shim.js', `${pathToFileURL(profilesDir).href}/`).href

registerHooks({
  resolve(specifier, context, nextResolve) {
    if (specifier.startsWith('@deepseek-ai/')) {
      try {
        return nextResolve(specifier, { ...context, parentURL: shimParent })
      } catch {
        // 安装树里没有：落回默认解析（仓库本地的未来 vendored 副本也能生效）
      }
    }
    return nextResolve(specifier, context)
  },
})
