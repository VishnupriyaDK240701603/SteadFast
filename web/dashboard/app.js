// Steadfast vs Baseline Dashboard Client
// No external dependencies or CDNs

(function() {
  'use strict';

  // State
  let steadfastThroughputHistory = [];
  let baselineThroughputHistory = [];
  const maxHistoryPoints = 30;

  // Query params: ?operator=1, ?present=1
  const urlParams = new URLSearchParams(window.location.search);
  const isOperator = urlParams.get('operator') === '1';
  const isPresent = urlParams.get('present') === '1';

  if (isPresent) {
    document.body.classList.add('present-mode');
  }

  // HTML escaping utility to prevent XSS / raw injection
  function escapeHTML(str) {
    if (str === null || str === undefined) return '';
    const div = document.createElement('div');
    div.textContent = String(str);
    return div.innerHTML;
  }

  // Setup Canvas Charts
  const stCanvas = document.getElementById('chart-steadfast-throughput');
  const blCanvas = document.getElementById('chart-baseline-throughput');
  const stCtx = stCanvas ? stCanvas.getContext('2d') : null;
  const blCtx = blCanvas ? blCanvas.getContext('2d') : null;

  function drawThroughputChart(ctx, data, strokeColor, fillColor) {
    if (!ctx) return;
    const w = ctx.canvas.width;
    const h = ctx.canvas.height;
    ctx.clearRect(0, 0, w, h);

    if (data.length === 0) return;

    const maxVal = Math.max(10, ...data);
    const step = w / Math.max(1, (maxHistoryPoints - 1));

    ctx.beginPath();
    ctx.moveTo(0, h);

    data.forEach((val, i) => {
      const x = i * step;
      const y = h - (val / maxVal) * (h - 10) - 5;
      if (i === 0) {
        ctx.lineTo(x, y);
      } else {
        ctx.lineTo(x, y);
      }
    });

    ctx.strokeStyle = strokeColor;
    ctx.lineWidth = 2;
    ctx.stroke();

    // Area fill
    ctx.lineTo((data.length - 1) * step, h);
    ctx.lineTo(0, h);
    ctx.fillStyle = fillColor;
    ctx.fill();
  }

  // SSE Stream
  function initSSE() {
    const evtSource = new EventSource('/api/stream');

    evtSource.onmessage = function(event) {
      try {
        const data = JSON.parse(event.data);
        updateDashboard(data);
      } catch (err) {
        console.error('Error parsing SSE data:', err);
      }
    };

    evtSource.onerror = function() {
      const stDbBadge = document.getElementById('st-db-status');
      if (stDbBadge) {
        stDbBadge.textContent = 'Disconnected';
        stDbBadge.className = 'db-status-badge status-down';
      }
    };
  }

  function updateDashboard(data) {
    // 1. Steadfast Engine Metrics
    const st = data.steadfast || {};
    const stDbBadge = document.getElementById('st-db-status');
    if (stDbBadge) {
      if (st.db_down) {
        stDbBadge.textContent = 'DATABASE DOWN';
        stDbBadge.className = 'db-status-badge status-down';
      } else {
        stDbBadge.textContent = 'DB Connected';
        stDbBadge.className = 'db-status-badge';
      }
    }

    setText('st-queue-depth', st.queue_depth || 0);
    setText('st-in-flight', st.in_flight || 0);
    setText('st-succeeded', st.succeeded || 0);
    setText('st-dead-count', st.dead_count || 0);
    setText('dlq-count-badge', st.dead_count || 0);
    setText('st-workers', st.worker_count || 0);

    const breakerEl = document.getElementById('st-breaker');
    if (breakerEl) {
      const state = (st.breaker_state || 'CLOSED').toUpperCase();
      breakerEl.textContent = state;
      breakerEl.className = 'metric-num badge-state' + (state === 'OPEN' ? ' state-open' : '');
    }

    setText('st-p50', (st.p50_ms || 0) + 'ms');
    setText('st-p99', (st.p99_ms || 0) + 'ms');

    // Steadfast Throughput Chart
    const stThroughput = st.throughput || 0;
    steadfastThroughputHistory.push(stThroughput);
    if (steadfastThroughputHistory.length > maxHistoryPoints) {
      steadfastThroughputHistory.shift();
    }
    drawThroughputChart(stCtx, steadfastThroughputHistory, '#3fb950', 'rgba(63, 185, 80, 0.15)');

    // 2. Baseline Metrics
    const bl = data.baseline || {};
    const blRedisBadge = document.getElementById('bl-redis-status');
    if (blRedisBadge) {
      if (bl.redis_down) {
        blRedisBadge.textContent = 'REDIS DOWN';
        blRedisBadge.className = 'db-status-badge status-down';
      } else {
        blRedisBadge.textContent = 'Redis Connected';
        blRedisBadge.className = 'db-status-badge';
      }
    }

    setText('bl-queue-depth', bl.queue_depth || 0);
    setText('bl-processing', bl.processing || 0);
    setText('bl-completed', bl.completed || 0);
    setText('bl-failed', bl.failed || 0);

    // Baseline Throughput Chart
    const blThroughput = bl.throughput || 0;
    baselineThroughputHistory.push(blThroughput);
    if (baselineThroughputHistory.length > maxHistoryPoints) {
      baselineThroughputHistory.shift();
    }
    drawThroughputChart(blCtx, baselineThroughputHistory, '#f85149', 'rgba(248, 81, 73, 0.15)');

    // 3. Stakes Counters
    const stakes = data.stakes || {};
    setText('st-double-charged', stakes.steadfast_double_charges || 0);
    setText('st-orders-lost', stakes.steadfast_orders_lost || 0);
    setText('st-dollars-lost', '$' + Number(stakes.steadfast_dollars_lost || 0).toFixed(2));

    setText('bl-double-charged', stakes.baseline_double_charges || 0);
    setText('bl-orders-lost', stakes.baseline_orders_lost || 0);
    setText('bl-dollars-lost', '$' + Number(stakes.baseline_dollars_lost || 0).toFixed(2));

    // 4. Verdict Strip
    const verdict = data.verdict;
    const badge = document.getElementById('verdict-badge');
    if (verdict && badge) {
      if (verdict.verdict === 'PASS') {
        badge.textContent = 'PASS';
        badge.className = 'verdict-badge badge-pass';
      } else if (verdict.verdict === 'FAIL') {
        badge.textContent = 'FAIL';
        badge.className = 'verdict-badge badge-fail';
      } else {
        badge.textContent = verdict.verdict || 'IDLE';
        badge.className = 'verdict-badge badge-idle';
      }

      // Invariants
      setInvClass('inv-i1', verdict.i1_pass);
      setInvClass('inv-i2', verdict.i2_pass);
      setInvClass('inv-i2b', verdict.i2b_pass);
      setInvClass('inv-i3', verdict.i3_pass);
      setInvClass('inv-i4', verdict.i4_pass);
    }
  }

  function setInvClass(id, isPass) {
    const el = document.getElementById(id);
    if (!el) return;
    if (isPass === true) {
      el.className = 'inv-item pass';
    } else if (isPass === false) {
      el.className = 'inv-item fail';
    } else {
      el.className = 'inv-item';
    }
  }

  function setText(id, val) {
    const el = document.getElementById(id);
    if (el) el.textContent = val;
  }

  // Demo Controls
  window.runDemo = function() {
    const select = document.getElementById('select-profile');
    const profile = select ? select.value : 'short';
    fetch('/api/demo?profile=' + encodeURIComponent(profile), { method: 'POST' })
      .then(res => res.json())
      .catch(err => console.error('Demo trigger error:', err));
  };

  window.triggerBeat = function(beat) {
    fetch('/api/demo/beat?beat=' + encodeURIComponent(beat), { method: 'POST' })
      .then(res => res.json())
      .catch(err => console.error('Beat trigger error:', err));
  };

  window.triggerChaos = function(action) {
    fetch('/api/chaos?action=' + encodeURIComponent(action), { method: 'POST' })
      .then(res => res.json())
      .catch(err => console.error('Chaos trigger error:', err));
  };

  // Keyboard Shortcuts (1-4 in operator mode or always when not typing)
  window.addEventListener('keydown', function(e) {
    if (['INPUT', 'TEXTAREA'].includes(document.activeElement.tagName)) return;
    if (['1', '2', '3', '4'].includes(e.key)) {
      triggerBeat(parseInt(e.key, 10));
    }
  });

  // DLQ Drawer Logic
  window.toggleDLQDrawer = function() {
    const drawer = document.getElementById('dlq-drawer');
    if (!drawer) return;
    drawer.classList.toggle('open');
    if (drawer.classList.contains('open')) {
      loadDLQJobs();
    }
  };

  function loadDLQJobs() {
    const container = document.getElementById('dlq-jobs-container');
    if (!container) return;
    container.innerHTML = '<p class="placeholder-text">Loading DLQ jobs...</p>';

    fetch('/api/dlq')
      .then(res => res.json())
      .then(jobs => {
        if (!jobs || jobs.length === 0) {
          container.innerHTML = '<p class="placeholder-text">No dead jobs in DLQ.</p>';
          return;
        }

        container.innerHTML = '';
        jobs.forEach(job => {
          const card = document.createElement('div');
          card.className = 'dlq-job-card';
          card.innerHTML = `
            <div><strong>Job ID:</strong> <span class="code-span">${escapeHTML(job.id)}</span></div>
            <div><strong>Type:</strong> ${escapeHTML(job.job_type)}</div>
            <div><strong>Attempts:</strong> ${escapeHTML(job.attempts)}</div>
            <div><strong>Failed At:</strong> ${escapeHTML(job.updated_at || job.created_at)}</div>
          `;
          card.onclick = () => selectDLQJob(job);
          container.appendChild(card);
        });
      })
      .catch(err => {
        container.innerHTML = `<p class="error-msg">Error loading DLQ: ${escapeHTML(err.message)}</p>`;
      });
  }

  function selectDLQJob(job) {
    const editor = document.getElementById('dlq-editor-panel');
    if (!editor) return;
    editor.style.display = 'block';

    document.getElementById('edit-job-id').value = job.id;
    document.getElementById('display-job-id').textContent = job.id;

    let payloadStr = '';
    try {
      payloadStr = JSON.stringify(JSON.parse(job.payload), null, 2);
    } catch (_) {
      payloadStr = job.payload || '';
    }
    document.getElementById('edit-job-payload').value = payloadStr;
    document.getElementById('json-error').textContent = '';

    // Attempts History
    const historyContainer = document.getElementById('attempts-history-container');
    if (historyContainer) {
      if (job.attempts_history && job.attempts_history.length > 0) {
        let historyHTML = '<h4>Attempt History:</h4>';
        job.attempts_history.forEach((att, idx) => {
          historyHTML += `
            <div style="margin-top:6px; font-size:0.8rem; background:#10141a; padding:6px; border-radius:4px;">
              <div><strong>#${idx + 1}</strong>: ${escapeHTML(att.error || 'Unknown error')}</div>
              <div style="color:#8b949e;">At: ${escapeHTML(att.timestamp)}</div>
            </div>
          `;
        });
        historyContainer.innerHTML = historyHTML;
      } else {
        historyContainer.innerHTML = '<p style="font-size:0.8rem; color:#8b949e;">No detailed attempt history recorded.</p>';
      }
    }
  }

  window.saveAndReplayJob = function() {
    const jobId = document.getElementById('edit-job-id').value;
    const payloadText = document.getElementById('edit-job-payload').value;
    const errDiv = document.getElementById('json-error');

    // JSON Validation
    try {
      JSON.parse(payloadText);
      errDiv.textContent = '';
    } catch (e) {
      errDiv.textContent = 'Invalid JSON: ' + e.message;
      return;
    }

    fetch('/api/dlq/replay', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ job_id: jobId, payload: payloadText })
    })
      .then(res => {
        if (!res.ok) throw new Error('HTTP ' + res.status);
        return res.json();
      })
      .then(() => {
        alert('Job payload updated and replayed successfully!');
        loadDLQJobs();
        document.getElementById('dlq-editor-panel').style.display = 'none';
      })
      .catch(err => {
        errDiv.textContent = 'Failed to replay: ' + err.message;
      });
  };

  // Start SSE on load
  window.addEventListener('DOMContentLoaded', initSSE);
})();
