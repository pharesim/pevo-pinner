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

    async init() {
      await this.fetchPapers();
      await this.fetchStatus();
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
        // Fallback
        const el = document.createElement('textarea');
        el.value = text;
        document.body.appendChild(el);
        el.select();
        document.execCommand('copy');
        document.body.removeChild(el);
      });
    }
  }));
});
