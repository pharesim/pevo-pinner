document.addEventListener('alpine:init', () => {
  Alpine.data('pinnerApp', () => ({
    papers: [],
    search: '',
    disciplineFilter: '',
    typeFilter: '',
    loading: true,
    status: {
      total_discovered: 0,
      pinned_count: 0,
      next_refresh: ''
    },
    _refreshSeconds: 0,
    _countdownTimer: null,

    // Tab state
    tab: 'papers',

    // Auto-pin rules
    rules: [],
    rulesLoading: false,
    showRuleForm: false,
    editingRule: null,
    evaluateResult: '',
    ruleForm: {
      name: '',
      disciplines: '',
      authors: '',
      cid_types: [],
      title_pattern: '',
      created_after: '',
      created_before: '',
      enabled: true
    },

    async init() {
      await this.fetchPapers();
      await this.fetchStatus();
      await this.fetchRules();
      this.loading = false;
      this._startCountdown();
    },

    _parseDuration(s) {
      var sec = 0;
      var m = s.match(/(\d+)h/);
      if (m) sec += parseInt(m[1], 10) * 3600;
      m = s.match(/(\d+)m/);
      if (m) sec += parseInt(m[1], 10) * 60;
      m = s.match(/(\d+)s/);
      if (m) sec += parseInt(m[1], 10);
      return sec;
    },

    _formatCountdown(sec) {
      if (sec <= 0) return '0s';
      var h = Math.floor(sec / 3600);
      var m = Math.floor((sec % 3600) / 60);
      var s = sec % 60;
      if (h > 0) return h + 'h' + m + 'm';
      if (m > 0) return m + 'm' + s + 's';
      return s + 's';
    },

    _startCountdown() {
      if (this._countdownTimer) clearInterval(this._countdownTimer);
      this._countdownTimer = setInterval(() => {
        if (this._refreshSeconds > 0) {
          this._refreshSeconds--;
          this.status.next_refresh = this._formatCountdown(this._refreshSeconds);
        } else {
          this.fetchPapers();
          this.fetchStatus();
        }
      }, 1000);
    },

    get disciplines() {
      const set = new Set();
      this.papers.forEach(p => {
        if (p.discipline) set.add(p.discipline);
      });
      return [...set].sort();
    },

    get filteredPapers() {
      const q = this.search.toLowerCase();
      return this.papers.filter(p => {
        if (this.disciplineFilter && p.discipline !== this.disciplineFilter) return false;
        if (this.typeFilter && p.cid_type !== this.typeFilter) return false;
        if (q && !p.title.toLowerCase().includes(q) && !p.author.toLowerCase().includes(q)) return false;
        return true;
      });
    },

    async fetchPapers() {
      try {
        const resp = await fetch('/api/papers');
        const data = await resp.json();
        this.papers = data.map(p => ({ ...p, _busy: false }));
      } catch (e) {
        console.error('Failed to fetch papers:', e);
      }
    },

    async fetchStatus() {
      try {
        const resp = await fetch('/api/status');
        this.status = await resp.json();
        this._refreshSeconds = this._parseDuration(this.status.next_refresh || '');
      } catch (e) {
        console.error('Failed to fetch status:', e);
      }
    },

    async togglePin(paper) {
      paper._busy = true;
      const endpoint = paper.pinned ? `/api/unpin/${paper.cid}` : `/api/pin/${paper.cid}`;
      try {
        const resp = await fetch(endpoint, { method: 'POST' });
        if (resp.ok) {
          paper.pinned = !paper.pinned;
        } else {
          const err = await resp.text();
          alert(`Operation failed: ${err}`);
        }
      } catch (e) {
        alert(`Network error: ${e.message}`);
      } finally {
        paper._busy = false;
        await this.fetchStatus();
      }
    },

    async pinAll() {
      this.loading = true;
      try {
        const resp = await fetch('/api/pin-all', { method: 'POST' });
        if (resp.ok) {
          await this.fetchPapers();
        }
      } catch (e) {
        alert(`Pin all failed: ${e.message}`);
      } finally {
        await this.fetchStatus();
        this.loading = false;
      }
    },

    async unpinAll() {
      if (!confirm('Unpin all CIDs? This will remove all locally stored content.')) return;
      this.loading = true;
      try {
        for (const paper of this.papers) {
          if (paper.pinned) {
            await fetch(`/api/unpin/${paper.cid}`, { method: 'POST' });
          }
        }
        await this.fetchPapers();
      } catch (e) {
        alert(`Unpin all failed: ${e.message}`);
      } finally {
        await this.fetchStatus();
        this.loading = false;
      }
    },

    copyToClipboard(text) {
      navigator.clipboard.writeText(text).catch(() => {
        const el = document.createElement('textarea');
        el.value = text;
        document.body.appendChild(el);
        el.select();
        document.execCommand('copy');
        document.body.removeChild(el);
      });
    },

    // --- Auto-pin rules ---

    async fetchRules() {
      this.rulesLoading = true;
      try {
        const resp = await fetch('/api/autopin/rules');
        this.rules = await resp.json();
      } catch (e) {
        console.error('Failed to fetch rules:', e);
      } finally {
        this.rulesLoading = false;
      }
    },

    resetRuleForm() {
      this.ruleForm = {
        name: '',
        disciplines: '',
        authors: '',
        cid_types: [],
        title_pattern: '',
        created_after: '',
        created_before: '',
        enabled: true
      };
    },

    editRule(rule) {
      this.editingRule = rule;
      this.ruleForm = {
        name: rule.name || '',
        disciplines: (rule.disciplines || []).join(', '),
        authors: (rule.authors || []).join(', '),
        cid_types: [...(rule.cid_types || [])],
        title_pattern: rule.title_pattern || '',
        created_after: rule.created_after || '',
        created_before: rule.created_before || '',
        enabled: rule.enabled
      };
      this.showRuleForm = true;
    },

    _buildRulePayload() {
      return {
        name: this.ruleForm.name.trim(),
        enabled: this.ruleForm.enabled,
        disciplines: this.ruleForm.disciplines ? this.ruleForm.disciplines.split(',').map(s => s.trim()).filter(Boolean) : [],
        authors: this.ruleForm.authors ? this.ruleForm.authors.split(',').map(s => s.trim()).filter(Boolean) : [],
        cid_types: this.ruleForm.cid_types,
        title_pattern: this.ruleForm.title_pattern.trim(),
        created_after: this.ruleForm.created_after,
        created_before: this.ruleForm.created_before
      };
    },

    async saveRule() {
      const payload = this._buildRulePayload();
      if (!payload.name) {
        alert('Rule name is required');
        return;
      }
      try {
        let resp;
        if (this.editingRule) {
          resp = await fetch(`/api/autopin/rules/${this.editingRule.id}`, {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(payload)
          });
        } else {
          resp = await fetch('/api/autopin/rules', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(payload)
          });
        }
        if (!resp.ok) {
          const err = await resp.text();
          alert(`Failed to save rule: ${err}`);
          return;
        }
        this.showRuleForm = false;
        this.editingRule = null;
        await this.fetchRules();
      } catch (e) {
        alert(`Error saving rule: ${e.message}`);
      }
    },

    async deleteRule(rule) {
      if (!confirm(`Delete rule "${rule.name}"?`)) return;
      try {
        const resp = await fetch(`/api/autopin/rules/${rule.id}`, { method: 'DELETE' });
        if (!resp.ok) {
          alert('Failed to delete rule');
          return;
        }
        await this.fetchRules();
      } catch (e) {
        alert(`Error deleting rule: ${e.message}`);
      }
    },

    async toggleRuleEnabled(rule) {
      const payload = { ...rule, enabled: !rule.enabled };
      try {
        const resp = await fetch(`/api/autopin/rules/${rule.id}`, {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(payload)
        });
        if (resp.ok) {
          await this.fetchRules();
        }
      } catch (e) {
        alert(`Error toggling rule: ${e.message}`);
      }
    },

    async enableAllRules() {
      try {
        const resp = await fetch('/api/autopin/enable-all', { method: 'POST' });
        if (resp.ok) this.rules = await resp.json();
      } catch (e) {
        alert(`Error: ${e.message}`);
      }
    },

    async disableAllRules() {
      try {
        const resp = await fetch('/api/autopin/disable-all', { method: 'POST' });
        if (resp.ok) this.rules = await resp.json();
      } catch (e) {
        alert(`Error: ${e.message}`);
      }
    },

    async evaluateRules() {
      this.evaluateResult = '';
      this.rulesLoading = true;
      try {
        const resp = await fetch('/api/autopin/evaluate', { method: 'POST' });
        const data = await resp.json();
        this.evaluateResult = `Matched ${data.matched} CIDs, pinned ${data.pinned} new` + (data.failed > 0 ? `, ${data.failed} failed` : '');
        await this.fetchPapers();
        await this.fetchStatus();
      } catch (e) {
        this.evaluateResult = `Evaluation failed: ${e.message}`;
      } finally {
        this.rulesLoading = false;
        setTimeout(() => { this.evaluateResult = ''; }, 5000);
      }
    }
  }));
});
