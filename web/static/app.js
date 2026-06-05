/* deepx web dashboard — Vue 3 global build, 零构建。
   通过 SSE 镜像终端会话,POST 回注输入 / review 确认。 */
const { createApp } = Vue;

marked.setOptions({ breaks: true, gfm: true });

// 跟 TUI 同步的 web UI 文案。lang 由后端快照 / lang 事件同步(读 ~/.deepx/meta.json)。
const I18N = {
  zh: {
    connected: '已连接',
    reconnecting: '重连中…',
    streaming: '生成中…',
    you: '你',
    send: '发送',
    'placeholder.idle': '输入消息,Enter 发送,Shift+Enter 换行',
    'placeholder.streaming': '正在生成…(可继续输入,发送后排队)',
    'review.title': '需要确认',
    'review.approve': '批准',
    'review.reject': '拒绝',
    workspace: '工作区',
    'panel.vendor': '模型厂商',
    'panel.curmodel': '当前模型',
    'panel.context': '上下文',
    'panel.todo': '待办',
    'panel.plan': '计划',
    'panel.tools': '工具调用',
    'label.used': '占用',
    'label.output': '输出',
    'label.cache': '缓存',
    'session.new': '＋ 新建',
    'session.untitled': '未命名',
    'session.rename': '重命名',
    'session.delete': '删除',
    'session.rename.prompt': '输入新的会话名称:',
    'session.delete.confirm': '确定删除这个会话吗?此操作不可撤销。',
    'panel.routing': '模型路由',
    'panel.mode': '权限模式',
    'panel.sandbox': '沙箱',
    'panel.workingmode': '工作模式',
    'panel.codegraph': '代码图谱',
    'cg.idle': '—',
    'cg.loading': '加载',
    'cg.ready': '就绪',
    'cg.stale': '更新',
    'cg.disabled': '已禁用',
    'cg.degraded': '降级',
  },
  en: {
    connected: 'connected',
    reconnecting: 'reconnecting…',
    streaming: 'streaming…',
    you: 'You',
    send: 'Send',
    'placeholder.idle': 'Type a message — Enter to send, Shift+Enter for newline',
    'placeholder.streaming': 'Generating… (you can keep typing; it queues after send)',
    'review.title': 'Confirmation needed',
    'review.approve': 'Approve',
    'review.reject': 'Reject',
    workspace: 'Workspace',
    'panel.vendor': 'Vendor',
    'panel.curmodel': 'Model',
    'panel.context': 'Context',
    'panel.todo': 'Todo',
    'panel.plan': 'Plan',
    'panel.tools': 'Tool Calls',
    'label.used': 'Used',
    'label.output': 'Output',
    'label.cache': 'Cache',
    'session.new': '＋ New',
    'session.untitled': 'untitled',
    'session.rename': 'Rename',
    'session.delete': 'Delete',
    'session.rename.prompt': 'Enter a new conversation name:',
    'session.delete.confirm': 'Delete this conversation? This cannot be undone.',
    'panel.routing': 'Routing',
    'panel.mode': 'Permission',
    'panel.sandbox': 'Sandbox',
    'panel.workingmode': 'Working Mode',
    'panel.codegraph': 'CodeGraph',
    'cg.idle': '—',
    'cg.loading': 'loading',
    'cg.ready': 'ready',
    'cg.stale': 'stale',
    'cg.disabled': 'disabled',
    'cg.degraded': 'degraded',
  },
};

// 控制项可选值(与 TUI / 后端一致)。
const ROUTING_OPTIONS = ['auto', 'flash', 'pro'];
const MODE_OPTIONS = ['plan', 'auto', 'review'];
const SANDBOX_OPTIONS = ['off', 'native', 'docker'];
const WORKING_MODE_OPTIONS = ['karpathy', 'openspec', 'superpowers'];

createApp({
  data() {
    return {
      messages: [],
      plan: [],
      step: [],
      toolCalls: [],
      usage: null,
      streaming: false,
      models: { flash: '', pro: '', activeRole: 'flash' },
      workspace: '',
      reviewPending: null,
      input: '',
      connected: false,
      openIdx: -1, // 当前流式 assistant 消息下标
      lang: 'zh',  // 跟 TUI 同步
      // 控制态(对齐 TUI),由快照 / 增量事件同步
      vendor: '',
      routing: 'auto',
      mode: 'review',
      sandbox: 'native',
      workingMode: 'karpathy',
      codegraph: '',
      sessions: [],
      menuOpen: null, // 当前展开"…"菜单的会话 id
      routingOptions: ROUTING_OPTIONS,
      modeOptions: MODE_OPTIONS,
      sandboxOptions: SANDBOX_OPTIONS,
      workingModeOptions: WORKING_MODE_OPTIONS,
      // @ 文件提及选择器
      mention: { active: false, idx: 0, query: '', start: 0, end: 0, hidden: false },
      mentionFiles: [],       // /api/files 拉到的工作区文件列表(懒加载、缓存)
      mentionFilesLoaded: false,
    };
  },
  computed: {
    // 已流式但还没出 token 时显示打字动画
    thinking() {
      return this.streaming && this.openIdx < 0;
    },
    // 当前实际在用的模型名(按活跃角色取 flash / pro)
    activeModel() {
      return this.models.activeRole === 'pro' ? this.models.pro : this.models.flash;
    },
    cacheRate() {
      // 显示格式:百分比 + 原始 (hit/prompt),一眼可核对。
      // 百分比跟 TUI 一致用整数除法截断(不四舍五入)。
      if (!this.usage || !this.usage.promptTokens) return '—';
      const hit = this.usage.cacheHit;
      const prompt = this.usage.promptTokens;
      return Math.floor((hit * 100) / prompt) + '% (' + hit + '/' + prompt + ')';
    },
    // @ 选择器当前候选(按 query 过滤,最多 10 条),跟 TUI filterWorkspaceFiles 同口径。
    mentionMatches() {
      if (!this.mention.active) return [];
      return this.filterFiles(this.mention.query, this.mentionFiles, 10);
    },
  },
  methods: {
    t(key) {
      const dict = I18N[this.lang] || I18N.zh;
      return dict[key] || (I18N.zh[key] || key);
    },
    render(text) {
      try { return marked.parse(text || ''); } catch (_) { return text || ''; }
    },
    // 代码图谱状态 token → 可读标签(对齐 TUI 的 codegraph.* 文案)
    cgLabel(s) {
      return s ? (this.t('cg.' + s) || s) : '';
    },
    planIcon(s) {
      return { done: '✓', running: '▶', failed: '✗', blocked: '⏸', pending: ' ' }[s] || ' ';
    },
    toolIcon(s) {
      return { done: '✓', running: '▶', failed: '✗' }[s] || '·';
    },
    // mainArg 从工具 args JSON 里抽一个最具代表性的字段,显示在 summary 行(对齐 TUI 的
    // extractMainArg)。完整 args 仍在展开后的 <pre> 里。解析失败 / 无字段返回空串。
    mainArg(tc) {
      if (!tc.args) return '';
      let a;
      try { a = JSON.parse(tc.args); } catch (_) { return ''; }
      if (!a || typeof a !== 'object') return '';
      let v = '';
      switch (tc.name) {
        case 'Read': case 'Write': case 'Update': case 'List': case 'Tree': case 'OCR':
          v = a.path; break;
        case 'Glob':
          v = a.path && a.path !== '.' ? `${a.pattern} in ${a.path}` : a.pattern; break;
        case 'Grep':
          v = a.path ? `${a.pattern} in ${a.path}` : a.pattern; break;
        case 'Search': v = a.query; break;
        case 'Fetch': v = a.url; break;
        case 'LoadSkill': v = a.name; break;
        case 'Bash': v = a.command; break;
        case 'SwitchModel': v = a.reason; break;
        case 'Memory': v = Array.isArray(a.keywords) ? a.keywords.join(' ') : ''; break;
        case 'UpdatePlanStatus':
          v = a.id && a.status ? `${a.id} → ${a.status}` : (a.id || ''); break;
        case 'CreatePlan': v = Array.isArray(a.plans) ? `${a.plans.length} nodes` : ''; break;
        default: v = a.path || '';
      }
      v = (v == null ? '' : String(v)).replace(/\s+/g, ' ').trim();
      return v.length > 80 ? v.slice(0, 77) + '…' : v;
    },
    scrollDown() {
      this.$nextTick(() => {
        const el = this.$refs.msgList;
        if (el) el.scrollTop = el.scrollHeight;
      });
    },
    onKey(e) {
      // @ 选择器激活时,方向键 / Tab / Enter / Esc 优先给选择器,不触发发送 / 换行。
      if (this.mention.active && this.mentionMatches.length) {
        if (e.key === 'ArrowDown') { e.preventDefault(); this.mentionMove(1); return; }
        if (e.key === 'ArrowUp') { e.preventDefault(); this.mentionMove(-1); return; }
        // Enter = 确认并关闭(文件 / 目录都选定);Tab = 对目录下钻(留在打开态)
        if (e.key === 'Enter' && !e.isComposing) { e.preventDefault(); this.mentionPick(this.mention.idx, true); return; }
        if (e.key === 'Tab') { e.preventDefault(); this.mentionPick(this.mention.idx, false); return; }
        if (e.key === 'Escape') {
          e.preventDefault(); this.mention.hidden = true; this.mention.active = false; return;
        }
      }
      if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) {
        e.preventDefault();
        this.send();
      }
    },

    // === @ 文件提及 ===

    // mentionContext 从光标往回扫,定位正在输入的 "@query"(镜像 TUI fileMentionContext)。
    // @ 须在串首或紧跟空白(避免 user@host 邮箱误判),且 @ 到光标间无空白。
    mentionContext(value, cursor) {
      for (let i = cursor - 1; i >= 0; i--) {
        const ch = value[i];
        if (ch === '@') {
          const prev = i > 0 ? value[i - 1] : '';
          if (i === 0 || /\s/.test(prev)) {
            return { active: true, start: i, end: cursor, query: value.slice(i + 1, cursor) };
          }
          return { active: false };
        }
        if (/\s/.test(ch)) return { active: false };
      }
      return { active: false };
    },
    filterFiles(query, files, limit) {
      const q = (query || '').toLowerCase();
      const pref = [], sub = [];
      for (const f of files) {
        const lf = f.toLowerCase();
        const base = (lf.endsWith('/') ? lf.slice(0, -1) : lf).split('/').pop();
        if (!q || lf.startsWith(q) || base.startsWith(q)) pref.push(f);
        else if (lf.includes(q)) sub.push(f);
      }
      return pref.concat(sub).slice(0, limit);
    },
    async ensureFiles() {
      if (this.mentionFilesLoaded) return;
      this.mentionFilesLoaded = true;
      try {
        const r = await fetch('/api/files');
        if (r.ok) this.mentionFiles = await r.json();
      } catch (_) { /* 拉不到就空列表,选择器不显示 */ }
    },
    // syncMention 据输入框当前值 + 光标重算提及态。keyup / input / click 时调用。
    onInput() { this.mention.hidden = false; this.syncMention(); },
    syncMention() {
      const ta = this.$refs.ta;
      if (!ta) return;
      const ctx = this.mentionContext(this.input, ta.selectionStart);
      if (!ctx.active || this.mention.hidden || this.input.startsWith('/')) {
        this.mention.active = false;
        return;
      }
      this.ensureFiles();
      this.mention.start = ctx.start;
      this.mention.end = ctx.end;
      this.mention.query = ctx.query;
      this.mention.active = true;
      const n = this.mentionMatches.length;
      if (this.mention.idx >= n) this.mention.idx = Math.max(0, n - 1);
      if (this.mention.idx < 0) this.mention.idx = 0;
    },
    mentionMove(d) {
      const n = this.mentionMatches.length;
      if (!n) return;
      this.mention.idx = Math.min(n - 1, Math.max(0, this.mention.idx + d));
    },
    // mentionPick 把光标处的 "@query" 替换成 "@<相对路径>"。
    //   - commit=true(Enter / 点击):文件 / 目录都补尾随空格并关闭选择器(确认选定)
    //   - commit=false(Tab):目录不补空格、留在打开态继续下钻;文件仍补空格关闭
    // 点击走 commit —— 点哪个选哪个,符合下拉直觉;目录下钻是键盘 Tab 的高级用法(或直接打字 narrow)。
    mentionPick(i, commit) {
      const matches = this.mentionMatches;
      if (!matches.length) return;
      const idx = (typeof i === 'number') ? i : this.mention.idx;
      const chosen = matches[idx];
      const drill = !commit && chosen.endsWith('/');
      const insert = '@' + chosen + (drill ? '' : ' ');
      this.input = this.input.slice(0, this.mention.start) + insert + this.input.slice(this.mention.end);
      const caret = this.mention.start + insert.length;
      this.mention.idx = 0;
      if (!drill) this.mention.active = false;
      this.$nextTick(() => {
        const ta = this.$refs.ta;
        if (ta) { ta.focus(); ta.setSelectionRange(caret, caret); if (drill) this.syncMention(); }
      });
    },
    async send() {
      const text = this.input.trim();
      if (!text) return;
      this.input = '';
      this.mention.active = false;
      await fetch('/api/input', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ text }),
      }).catch(() => {});
    },
    async review(approve) {
      await fetch('/api/review', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ approve }),
      }).catch(() => {});
    },

    // 用整份快照重置状态(新连接 / 重连)
    applySnapshot(s) {
      this.messages = s.messages || [];
      this.plan = s.plan || [];
      this.step = s.step || [];
      this.toolCalls = s.toolCalls || [];
      this.usage = s.usage || null;
      this.streaming = !!s.streaming;
      this.models = s.models || this.models;
      this.workspace = s.workspace || '';
      this.reviewPending = s.reviewPending || null;
      if (s.lang) this.lang = s.lang;
      if (s.vendor) this.vendor = s.vendor;
      if (s.routing) this.routing = s.routing;
      if (s.mode) this.mode = s.mode;
      if (s.sandbox) this.sandbox = s.sandbox;
      if (s.workingMode) this.workingMode = s.workingMode;
      this.codegraph = s.codegraph || '';
      this.sessions = s.sessions || [];
      // 推断流式气泡:最后一条是 assistant 且还在 streaming 则继续往里追加
      const last = this.messages.length - 1;
      this.openIdx = (this.streaming && last >= 0 && this.messages[last].role === 'assistant') ? last : -1;
      this.scrollDown();
    },

    // 应用单条增量事件(与后端 hub.apply 同构)
    applyEvent(ev) {
      switch (ev.kind) {
        case 'user_message':
          this.messages.push({ role: 'user', content: ev.text || '' });
          this.plan = [];
          this.step = [];
          this.toolCalls = [];
          this.usage = null;
          this.reviewPending = null;
          this.streaming = true;
          this.openIdx = -1;
          break;
        case 'token':
          if (this.openIdx < 0) {
            this.messages.push({ role: 'assistant', content: '' });
            this.openIdx = this.messages.length - 1;
          }
          this.messages[this.openIdx].content += ev.text || '';
          break;
        case 'reasoning_token':
          break; // 思考过程不入聊天
        case 'tool_call':
          this.toolCalls.push({ id: ev.id, name: ev.name, args: ev.args || '', status: 'running', output: '' });
          break;
        case 'tool_result': {
          const t = this.toolCalls.find((x) => x.id === ev.id) ||
            [...this.toolCalls].reverse().find((x) => x.name === ev.name && x.status === 'running');
          if (t) {
            t.status = ev.success ? 'done' : 'failed';
            t.output = ev.output || '';
          }
          break;
        }
        case 'model_switch':
          if (ev.role) this.models.activeRole = ev.role;
          break;
        case 'plan':
          // createplan → 步骤;todo / 其它 → 计划(对齐 TUI 与后端 hub)
          if (ev.planKind === 'createplan') this.step = ev.plan || [];
          else this.plan = ev.plan || [];
          break;
        case 'plan_status': {
          // 节点 id 可能在 计划 或 步骤 任一份里
          const p = this.plan.find((x) => x.id === ev.id) || this.step.find((x) => x.id === ev.id);
          if (p) {
            if (ev.status) p.status = ev.status;
            if (ev.summary) p.summary = ev.summary;
          }
          break;
        }
        case 'usage':
          this.usage = ev.usage || null;
          break;
        case 'done':
        case 'error':
          this.streaming = false;
          this.openIdx = -1;
          break;
        case 'review_request':
          this.reviewPending = { name: ev.name, args: ev.args || '' };
          break;
        case 'review_resolved':
          this.reviewPending = null;
          break;
        case 'lang':
          if (ev.text) this.lang = ev.text;
          break;
        case 'vendor':
          if (ev.text) this.vendor = ev.text;
          break;
        case 'routing':
          if (ev.text) this.routing = ev.text;
          break;
        case 'mode':
          if (ev.text) this.mode = ev.text;
          break;
        case 'sandbox':
          if (ev.text) this.sandbox = ev.text;
          break;
        case 'working_mode':
          if (ev.text) this.workingMode = ev.text;
          break;
        case 'codegraph':
          if (ev.text) this.codegraph = ev.text;
          break;
        case 'sessions':
          this.sessions = ev.sessions || [];
          break;
        case 'session_loaded':
          // 切换 / 新建会话:重置聊天与本轮派生状态。
          this.messages = ev.messages || [];
          this.plan = [];
          this.step = [];
          this.toolCalls = [];
          this.usage = null;
          this.reviewPending = null;
          this.streaming = false;
          this.openIdx = -1;
          break;
      }
      this.scrollDown();
    },

    // === 控制类:点按钮 POST 回 TUI,状态由后续 SSE 增量回灌(不本地乐观更新,保证与 TUI 一致)===
    async post(url, body) {
      await fetch(url, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: body ? JSON.stringify(body) : '{}',
      }).catch(() => {});
    },
    newSession() { this.post('/api/new'); },
    switchSession(id) { if (!this.streaming) this.post('/api/switch', { id }); },
    toggleMenu(id) { this.menuOpen = this.menuOpen === id ? null : id; },
    renameSession(s) {
      this.menuOpen = null;
      const title = window.prompt(this.t('session.rename.prompt'), s.title || '');
      if (title != null && title.trim()) this.post('/api/session-rename', { id: s.id, title: title.trim() });
    },
    deleteSession(s) {
      this.menuOpen = null;
      if (window.confirm(this.t('session.delete.confirm'))) this.post('/api/session-delete', { id: s.id });
    },
    setLang(lang) { this.post('/api/lang', { lang }); },
    setModel(role) { this.post('/api/model', { role }); },
    setMode(m) { this.post('/api/mode', { mode: m }); },
    setSandbox(m) { this.post('/api/sandbox', { mode: m }); },
    setWorkingMode(m) { this.post('/api/workingmode', { mode: m }); },

    connect() {
      const es = new EventSource('/api/events');
      es.addEventListener('snapshot', (e) => {
        this.connected = true;
        this.applySnapshot(JSON.parse(e.data));
      });
      es.addEventListener('delta', (e) => {
        this.applyEvent(JSON.parse(e.data));
      });
      es.onerror = () => { this.connected = false; };
    },
  },
  mounted() {
    this.connect();
    // 点菜单以外的任何地方关闭"…"菜单(菜单按钮 / 菜单本身用 @click.stop 不会冒泡到这里)。
    document.addEventListener('click', () => { this.menuOpen = null; });
  },
}).mount('#app');
