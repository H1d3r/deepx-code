package workflow

// preludeJS 在用户脚本之前以全局脚本执行,把对外的 workflow 全局 API 定义在
// 引擎注入的底层原语(__agentSync / __agentBatch / __log / __phase / __budget*)之上。
//
// 并发模型(关键):单个 qjs runtime 是单线程,跨 goroutine 触碰同一 wasm 实例会 race
// (已实测)。因此并发只允许发生在「一次同步 Go 调用内部」——JS 线程在该调用里阻塞,
// Go 侧用 worker goroutine 并发跑子 agent,跑完再一次性返回,全程不并发碰 wasm。
//
//   - agent(prompt, opts) 返回一个「延迟 thenable」:带着本次调用的 payload。
//     · 单独 await 时 → 触发 __agentSync,顺序执行一个;
//     · 被 parallel 收集时 → 读出各 payload,交给 __agentBatch 一次并发跑完(真并发)。
//   - parallel(thunks):收集各 deferred 的 payload → __agentBatch(并发)→ 回填结果;
//     单项失败收敛成 null(整批不 reject,对齐 pipeline / Claude Code,用 .filter(Boolean))。
//   - pipeline(items, ...stages):每个 item 顺序流过各 stage(stage 内的 agent 仍顺序);
//     item 间暂不并发(留作后续)。
const preludeJS = `
(function () {
  var g = globalThis;
  g.__wfPhase = "";

  g.phase = function (title) {
    g.__wfPhase = String(title == null ? "" : title);
    __phase(g.__wfPhase);
  };

  g.log = function (message) {
    __log(String(message == null ? "" : message));
  };

  function buildPayload(prompt, opts) {
    opts = opts || {};
    return JSON.stringify({
      prompt: String(prompt == null ? "" : prompt),
      label: opts.label || "",
      phase: opts.phase || g.__wfPhase || "",
      model: opts.model || "",
      schema: opts.schema || null,
      capabilities: opts.capabilities || null,
      max_tool_iters: opts.max_tool_iters || 0,
      max_tool_calls: opts.max_tool_calls || 0
    });
  }

  // agent 返回延迟 thenable:.then 触发时才同步执行一个(供单独 await / pipeline 顺序用);
  // parallel 则直接读 __wfPayload 走批处理,不触发 .then。
  g.agent = function (prompt, opts) {
    opts = opts || {};
    return {
      __wfPayload: buildPayload(prompt, opts),
      __wfSchema: !!opts.schema,
      then: function (onF, onR) {
        var p;
        try {
          var res = __agentSync(this.__wfPayload);
          p = Promise.resolve(this.__wfSchema ? JSON.parse(res) : res);
        } catch (e) {
          p = Promise.reject(e);
        }
        return p.then(onF, onR);
      },
      catch: function (onR) { return this.then(undefined, onR); },
      finally: function (cb) {
        return this.then(
          function (v) { cb(); return v; },
          function (e) { cb(); throw e; }
        );
      }
    };
  };

  function isDeferred(d) {
    return d && typeof d.__wfPayload === "string";
  }

  // parallel 的失败语义:单个 agent 失败 → 结果数组对应位置为 null,整批 promise **不 reject**
  // (与 pipeline 一致,也对齐 Claude Code:用 .filter(Boolean) 过滤)。这样一个视角挂掉不会
  // 拖垮其余跑完的结果。
  g.parallel = function (thunks) {
    var ds = (thunks || []).map(function (t) { return t(); });
    // 全是 agent deferred → 走并发批处理(真并发在 Go 侧)。
    if (ds.length > 0 && ds.every(isDeferred)) {
      var payloads = ds.map(function (d) { return d.__wfPayload; });
      var results = JSON.parse(__agentBatch(JSON.stringify(payloads)));
      return Promise.resolve(results.map(function (r, i) {
        if (!r.ok) return null; // 该 agent 失败 → null
        if (!ds[i].__wfSchema) return r.result;
        try { return JSON.parse(r.result); } catch (e) { return null; } // schema 结果非法 JSON 也算该项失败
      }));
    }
    // 混入了非 agent 的项 → 逐个解析(顺序);同样把失败项收敛成 null,不 reject。
    return Promise.all(ds.map(function (d) {
      return Promise.resolve(d).then(function (v) { return v; }, function () { return null; });
    }));
  };

  g.pipeline = function (items) {
    var stages = Array.prototype.slice.call(arguments, 1);
    return Promise.all((items || []).map(function (item, i) {
      var p = Promise.resolve(item);
      stages.forEach(function (stage) {
        p = p.then(function (v) { return stage(v, item, i); });
      });
      return p.catch(function () { return null; });
    }));
  };

  g.budget = {
    get total() { var t = __budgetTotal(); return t < 0 ? null : t; },
    spent: function () { return __budgetSpent(); },
    remaining: function () {
      var t = __budgetTotal();
      return t < 0 ? Infinity : Math.max(0, t - __budgetSpent());
    }
  };

  // 嵌套调用另一个 workflow(限一层):同步跑完子 workflow,resolve 出它的结果(字符串)。
  g.workflow = function (name, args) {
    try {
      var res = __workflow(String(name == null ? "" : name), JSON.stringify(args === undefined ? null : args));
      return Promise.resolve(res);
    } catch (e) {
      return Promise.reject(e);
    }
  };

  // 确定性:屏蔽随机/时间源。workflow 按 agent 执行序号缓存结果以支持 resume(中断重跑),
  // 非确定性会让重放时的调用序列错位、缓存失效。对齐 Claude Code 做法。(Math.max 等不受影响)
  Math.random = function () { throw new Error("workflow: Math.random() 不可用(会破坏 resume 确定性)"); };
  Date.now = function () { throw new Error("workflow: Date.now() 不可用(会破坏 resume 确定性)"); };
})();
`
