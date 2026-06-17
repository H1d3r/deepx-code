// 多视角代码审查 workflow 样例。
//
// 安装:拷到 .deepx/workflows/(项目级)或 ~/.deepx/workflows/(全局),然后在 deepx 里:
//   /workflow review-changes
//
// 说明:agent() 每次会起一个子 agent 干活;parallel() 让三个视角真并发;
// phase()/log() 驱动进度显示;main 的返回值作为本回合最终输出。

export const meta = {
  name: "review-changes",
  description: "从正确性/安全/可维护性三个视角审查当前改动并汇总",
  phases: [
    { title: "Review", detail: "三个视角并行审查" },
    { title: "Synthesize", detail: "汇总成最终结论" },
  ],
};

export default async function main(args) {
  phase("Review");
  log("启动三个视角的审查");

  const reviews = await parallel([
    () => agent(
      "先用 git diff(必要时 git diff --staged)看清当前未提交的改动,再从【正确性】角度审查:" +
      "找出真实 bug、边界遗漏、错误处理缺失。每条给出 文件:行号 + 一句话问题,按严重度排序;没有就说没有。",
      { label: "correctness", model: "pro" },
    ),
    () => agent(
      "先 git diff 看清当前未提交的改动,再从【安全】角度审查:命令注入、路径穿越、密钥泄露、" +
      "不安全的反序列化/执行。每条给 文件:行号 + 风险;没有就说没有。",
      { label: "security", model: "pro" },
    ),
    () => agent(
      "先 git diff 看清当前未提交的改动,再从【可维护性】角度审查:重复代码、命名、过度复杂、缺测试。" +
      "每条给 文件:行号 + 建议;没有就说没有。",
      { label: "maintainability", model: "pro" },
    ),
  ]);

  phase("Synthesize");
  return await agent(
    "把下面三份审查意见去重、按严重度合并成一份 markdown 报告(标题 + 分级要点,带 文件:行号)。" +
    "只输出报告正文,不要输出 JSON、不要客套:\n\n" +
    "【正确性】\n" + reviews[0] + "\n\n【安全】\n" + reviews[1] + "\n\n【可维护性】\n" + reviews[2],
    { label: "synthesizer", model: "pro" },
  );
}
