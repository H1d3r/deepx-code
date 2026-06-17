---
name: creating-workflows
description: 当用户想「创建/生成一个 workflow(可复用的多子-agent 编排脚本)」时使用。讲清 deepx workflow 的 JavaScript 脚本格式、可用 API,以及如何用 Workflow 工具保存与运行。
---

# 创建 workflow

deepx 的 workflow 是一段 **JavaScript 脚本**,用固定流程编排多个子 agent:脚本控制流程(循环、扇出、汇总),每个 `agent()` 调用做一次真正的 LLM 工作。脚本在 QuickJS 沙箱里跑,只能通过下列全局函数触达外界。

## 何时用

- 多视角审查、扇出研究、流水线处理、对抗式验证、循环到无新增——这类**固定可复用、会重复跑**的多步编排。
- 一次性的简单任务别用 workflow,直接做。

## 脚本格式(务必照此)

```javascript
export const meta = {
  name: "my-flow",              // 必须等于文件名(不含后缀 .mjs),kebab-case
  description: "一句话说明这个 workflow 做什么",
  phases: [                     // 可选,用于进度展示
    { title: "Collect", detail: "收集信息" },
    { title: "Report",  detail: "汇总成报告" },
  ],
};

export default async function main(args) {   // 入口必须是 default 导出的 async 函数
  phase("Collect");
  const data = await agent("收集关于 X 的信息", { label: "收集信息", model: "flash" });

  phase("Report");
  // 汇总/出报告这步用 pro(更稳),收集那步用默认 flash。label 用任务描述、跟随用户语言。
  return await agent("基于以下内容写报告:\n" + data, { label: "写报告", model: "pro" });
}
```

规则:
- 第一段(去注释后)必须是 `export const meta = {...}`,且 `meta.name` 等于文件名。
- 名字只能小写字母、数字、连字符(kebab-case)。
- 入口必须是 `export default async function main(args)`。

## 全局 API

- `agent(prompt, opts?)` → Promise:跑一个子 agent,resolve 出它的文本结果。
  `opts`:
  - `label`:进度显示名(单行显示在步骤里)。**写成简短的任务描述**(如「正确性审查」/「collect changes」),让用户一眼看懂这步在干嘛;**用与用户请求相同的语言**(用户用中文提需求就用中文 label,英文就英文)。**别用 `collector` / `step1` 这种没信息量的英文 ID**。
  - `model`:`"flash"`(默认,快、便宜,适合收集/检索/简单审查这类轻活)或 `"pro"`(更强,适合**汇总合并、复杂推理、写最终报告**这类重活)。**不写就是 flash。**
  - `phase`:归属阶段(配合 `phase()`)。
  - `schema`:JSON Schema,要结构化结果时用,resolve 出已解析的对象。

  ```javascript
  // 轻活:用 flash(省略 model 也是 flash)。label 写成任务描述、跟随用户语言。
  const data = await agent("收集 X 的资料", { label: "收集资料", model: "flash" });

  // 重活(汇总/出报告):显式 model: "pro"
  const report = await agent("把上面资料合并成 markdown 报告:\n" + data, {
    label: "写报告",
    model: "pro",
  });

  // 要结构化结果:加 schema(resolve 出已解析对象,不是字符串)
  const picks = await agent("列出 3 条改进建议", {
    label: "列改进建议",
    model: "pro",
    schema: {
      type: "object",
      required: ["items"],
      properties: {
        items: { type: "array", items: { type: "string" } },
      },
    },
  });
  // picks.items 是数组
  ```

> **选模型的经验法则**:扇出的并行子任务(收集、各视角初审)用默认 `flash`;最后**汇总/合并/出报告**那一步用 `{ model: "pro" }`——它最吃推理,用 flash 容易输出不稳(格式飘、甚至退化)。
- `parallel(thunks)` → Promise:**真并发**跑多个 agent(这是获得并发的唯一方式)。
  thunk 必须是 `() => agent(...)` 箭头函数,不是直接 `agent(...)`。
  ```javascript
  // 并行的初审/扇出子任务一般用 flash;label 用于进度显示
  const [a, b] = await parallel([
    () => agent("分析 A", { label: "分析A", model: "flash" }),
    () => agent("分析 B", { label: "分析B", model: "flash" }),
  ]);
  ```
- `pipeline(items, ...stages)` → Promise:每个 item 顺序流过各 stage(`(prev, item, i) => ...`)。
- `phase(title)`:标记当前阶段(驱动进度显示)。
- `log(message)`:打一行进度日志。
- `budget`:`budget.total` / `budget.spent()` / `budget.remaining()`。
- `args`:调用时传入的参数(`/workflow <名字> key=value` 或工具 args)。

> 想要并发就用 `parallel()`。直接 `Promise.all([agent(a), agent(b)])` 不会并发(会顺序跑)。

> **沙箱限制(务必遵守,否则脚本会抛错崩溃)**:脚本跑在 QuickJS 沙箱里,**只能用上面列出的全局函数** —— 没有 `require` / `import` / `fs` / `fetch` / `process`。并且为保证 resume(中断重跑)的确定性,**`Math.random()` 和 `Date.now()` 被禁用(调用即抛错)**;`new Date()`(取当前时间)虽未硬拦,但同样会破坏 resume 一致性,**也别用**。需要随机数 / 时间戳就用 `args` 从外面传进来,或让子 agent 在它自己的 prompt 里处理(子 agent 不受此限)。

## 分解粒度与并行度(重要)

默认**倾向于拆细、多并行**——这样更快、覆盖更全,也是 workflow 相对单个 agent 的核心价值:

- **能拆成独立子任务就拆,用 `parallel()` 同时跑**。例:审查代码别用一个 agent 全包,拆成 正确性 / 安全 / 性能 / 可维护性 多个视角并行;研究别串行,按子主题扇出。
- **按需求规模缩放**:
  - 简单 / 一次性需求 → 几个 agent 即可,别过度编排;
  - 「全面 / 彻底 / 多角度 / 审计」这类措辞 → **宽扇出**(更多并行 agent)+ 一个汇总/验证阶段(汇总那步记得 `model: "pro"`)。
- **但有硬上限,务必遵守**:
  - `parallel()` **同一时刻最多 8 个 agent 在跑**,多传的会自动排队——所以可以放心一次传几十个 thunk(不会爆,只是排队跑完);
  - **单次运行子 agent 总数上限 500**,超了会被中止。**绝不要写无界循环**(如 `while (true) await agent(...)`);要循环就用明确次数,或用 `budget.remaining()` 收敛(`while (budget.total && budget.remaining() > 50000) {...}`)。
- **真并发只有 `parallel()` 提供**;`pipeline()` 目前按 item 顺序跑、不跨 item 并发。要并发就 `parallel()`。
- **扇出步骤产「结构化紧凑结果」,别产长篇散文(重要)**:多个 review/分析 agent 用 `schema` 输出结构化清单(如 `{ findings: [{file,line,severity,issue}] }`),汇总步只在这些紧凑数据上去重/排序/格式化。**反例**:让每个扇出 agent 写几千上万字的长篇 markdown,再 `r.join()` 全塞给汇总 agent——输入会膨胀到几万字,模型容易退化(输出空 / `{}` / 被截断)。这是 Claude Code / 业界审查类 workflow 的标准做法。

## 两条容易踩的坑(务必遵守)

1. **`agent()` 的子 agent 自带全套工具权限**(Bash / Read / Grep / git 等)。需要「看代码改动、读文件、跑命令」时,**在 prompt 里让 agent 自己去做**(例如「先运行 `git diff` 看清未提交改动,再审查」),**不要**把 diff、文件内容这类大块数据当参数传进来。
2. **`args` 只用来传简单值**(如 `version`、`path`、`topic`),**不能传多行/大段内容**。原因:`/workflow <名> key=value` 的参数是按空格切的单行,无法塞进一个多行 diff;也没有 shell 展开。所以**别设计成 `args.diff` 这种依赖**——审查类 workflow 应让子 agent 自己 `git diff`。

3. **`main` 的返回值会作为最终结果按 markdown 渲染给用户**。所以**最后那个、返回给用户的** agent 输出 markdown 文本(标题/列表/粗体),`main` 返回它的文本(别返回对象 `{ ... }`)。
   - 注意区分:**中间的扇出步骤恰恰相反**——它们用 `schema` 产结构化数据喂给下游汇总(见上节「扇出产结构化结果」),只有**最终交付给用户**的那步才输出 markdown 文本。

## 如何保存与运行(用 Workflow 工具)

写好脚本后,**调用 `Workflow` 工具**:

- 创建:`Workflow({ action: "create", saveAs: "<kebab名>", script: "<完整脚本源码>" })`
  → 保存到 `.deepx/workflows/<名>.mjs`。
- 运行:`Workflow({ action: "run", name: "<名>", args: "<可选>" })`
  → 运行前会请用户确认(脚本是会真正干活的代码)。
- 列出:`Workflow({ action: "list" })`。

典型流程:先根据用户需求写出脚本 → `action:create` 保存 → 告诉用户可以用 `/workflow <名>` 或让你 `action:run` 运行。**不要**自己用 Write 工具去写 workflow 文件,统一走 `Workflow` 工具的 `create`。

## 形态一:扇出→汇总(审查 / 研究 / 多方案设计 / 多视角分析,同一形状)

下面以审查为例;**研究**(按子主题扇出)、**多方案设计**(各角度出方案再评选)、**多视角分析**都是同一结构——扇出步用 `schema` 产结构化结果,汇总步合并。

```javascript
export const meta = {
  name: "review-3",
  description: "三视角并发审查并汇总",
  phases: [                          // 声明阶段 → 运行前就把全部阶段列出来、逐步点亮(强烈建议声明)
    { title: "Review", detail: "三视角并行审查" },
    { title: "Synthesize", detail: "去重合并出报告" },
  ],
};

// 每个 review 只产「结构化问题清单」,不写长篇散文 —— 汇总步的输入因此很小、稳。
const FINDINGS = {
  type: "object",
  required: ["findings"],
  properties: {
    findings: { type: "array", items: {
      type: "object",
      required: ["severity", "issue"],
      properties: {
        file:     { type: "string" },
        line:     { type: "string" },
        severity: { type: "string", enum: ["high", "medium", "low"] },
        issue:    { type: "string" },   // 一句话问题
        fix:      { type: "string" },   // 一句话建议
      },
    }},
  },
};

export default async function main() {
  phase("Review");
  // label 是单行任务描述,用与用户请求相同的语言(此处用户用中文 → label 用中文;英文请求就用英文)。
  const [c, s, m] = await parallel([
    () => agent("先 git diff 看清改动,审【正确性与边界】,只输出问题清单", { label: "正确性审查", model: "flash", schema: FINDINGS }),
    () => agent("先 git diff 看清改动,审【安全风险】,只输出问题清单",     { label: "安全审查",   model: "flash", schema: FINDINGS }),
    () => agent("先 git diff 看清改动,审【可维护性】,只输出问题清单",     { label: "可维护性审查", model: "flash", schema: FINDINGS }),
  ]);

  phase("Synthesize");
  // 汇总只在紧凑的结构化数据上去重/排序(输入小 → pro 稳)。
  // 防御:schema 结果偶发可能不规范,取字段一律用 `?.` / `|| []` 兜底,避免 null.xxx 崩溃。
  const all = [...(c?.findings || []), ...(s?.findings || []), ...(m?.findings || [])];
  // 这步【故意不加 schema】:它要产「给用户看的 markdown 报告正文」,加 schema 会逼成 JSON 对象、
  // main 返回对象就渲染成一坨 JSON(空了就是 {})。最终交付步一律无 schema、输出 markdown 文本。
  return await agent(
    "把下面的发现去重合并、按严重度(high→medium→low)排序,输出 markdown 报告(标题 + 分级列表,带 文件:行号):\n" +
    JSON.stringify(all),
    { label: "汇总报告", model: "pro" },
  );
}
```

## 形态二:流水线 pipeline(一批 item 各自顺序过多个 stage)

适合「对一批东西逐个做同一串处理」:每个 item 独立地顺序流过各 stage,stage 回调收 `(上一步结果, 原 item, 下标)`。

```javascript
export const meta = { name: "triage-files", description: "对一批文件逐个分类并出建议" };
export default async function main(args) {
  const files = (args && args.files) || ["a.go", "b.go"]; // 数组需经 Workflow 工具的 JSON args 传;/workflow 名 k=v 传不了数组,没传就用默认
  const out = await pipeline(
    files,
    (f)       => agent("判定文件改动类型(bug/style/perf),只回一个词:" + f, { label: "分类", model: "flash" }),
    (cls, f)  => agent(`文件 ${f} 属于 ${cls},给一条最重要的改进建议(一句话)`, { label: "出建议", model: "flash" }),
  );
  return "## 文件 triage\n\n" + files.map((f, i) => `- **${f}**:${out[i]}`).join("\n");
}
```

## 形态三:循环到无新增 loop-until-dry(发现型任务,不知道总量)

适合「反复找,直到连续几轮没新东西」。**必须有明确轮数上限**(别写无界循环;总 agent 数上限 500)。

```javascript
export const meta = { name: "find-issues", description: "反复扫描直到没有新问题" };
const ISSUES = { type: "object", required: ["issues"],
  properties: { issues: { type: "array", items: { type: "string" } } } };

export default async function main() {
  const seen = [];
  let dry = 0;
  for (let round = 1; round <= 10 && dry < 2; round++) { // 上限 10 轮 + 连续 2 轮空则停
    const r = await agent(
      "找出尚未发现的问题(已找到:" + (seen.join("; ") || "无") + ")",
      { label: "第" + round + "轮扫描", model: "flash", schema: ISSUES },
    );
    const fresh = (r?.issues || []).filter(x => !seen.includes(x)); // 防御:schema 结果可能不规范
    if (fresh.length === 0) { dry++; continue; }
    dry = 0;
    seen.push(...fresh);
  }
  return `## 共发现 ${seen.length} 个问题\n\n` + seen.map(s => "- " + s).join("\n");
}
```
