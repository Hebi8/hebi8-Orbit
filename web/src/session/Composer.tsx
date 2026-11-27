import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode, type RefObject } from 'react';
import { createPortal } from 'react-dom';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Check, ChevronDown, MessageSquare, Pencil, RefreshCw } from 'lucide-react';
import type {
  AgentModeOption,
  AgentProbe,
  AnnotationView,
  BoardMemberCursor,
  ContextUsage,
  FollowupMode,
  MachineView,
  PendingInput,
  ProjectView,
  QuotaWindow,
  SessionAdapter,
  TaskSummary,
} from '../generated/protocol';
import { SessionAdapterValues } from '../generated/protocol';
import {
  PermAskIcon,
  PermAutoIcon,
  PermPlanIcon,
  PermYoloIcon,
  QueueSendIcon,
  SendUpIcon,
  StopSquareIcon,
  SwarmIcon,
} from '../components/Icons';
import { TaskRefButton, TaskRefChips } from '../components/TaskRefPicker';
import {
  ADAPTER_LABELS,
  classifyPermissionMode,
  defaultModeDisplayLabel,
  defaultModeFor,
  effortLabel,
  findAgentProbe,
  isFullBypassMode,
  modeLabel,
  nextModeOverrideAfterSend,
  probeEfforts,
  probeModels,
  probeModes,
  type PermissionIconKind,
} from '../lib/agentCaps';
import { api, errMsg } from '../lib/api';
import { PA_PROJECT_ID, formatTokenCount, isTaskBusy } from '../lib/format';
import { useImeGuard } from '../lib/ime';
import { useAnchoredPopover } from '../lib/popover';
import { formatQuotaResetTime, resolveAgentQuota } from '../lib/quota';
import { toast, useWorkbenchStore } from '../store/useWorkbenchStore';

export type ComposerIndicatorMode = 'context' | 'quota';

export interface SessionComposerProps {
  standalone?: false;
  task: TaskSummary;
  worker?: string;
  members: BoardMemberCursor[];
  editing: PendingInput | null;
  needsVerifyAttemptId?: string;
  onEditDone: () => void;
  openAnnotations?: AnnotationView[];
  runtimeConfig?: Record<string, string>;
  contextUsage?: ContextUsage | null;
  indicatorMode?: ComposerIndicatorMode;
  machines?: MachineView[];
  projects?: ProjectView[];
  machineId?: string;
  compact?: boolean;
  inDrawer?: boolean;
  placeholder?: string;
  topSlot?: ReactNode;
  draftInsert?: { id: number; text: string } | null;
  onOpenReview?: () => void;
}

export interface StandaloneComposerProps {
  standalone: true;
  value: string;
  onChange: (value: string) => void;
  onSubmit: () => void;
  placeholder?: string;
  autoFocus?: boolean;
  textareaRef?: RefObject<HTMLTextAreaElement | null>;
  canSend: boolean;
  submitting?: boolean;
  controlsDisabled?: boolean;
  indicatorMode?: ComposerIndicatorMode;
  contextUsage?: ContextUsage | null;
  availableAdapters: readonly SessionAdapter[];
  effectiveAdapter: SessionAdapter | '';
  onAdapterChange: (adapter: SessionAdapter) => void;
  activeProbe?: AgentProbe;
  effectiveModel: string;
  modelChoices: readonly string[];
  onModelChange: (model: string) => void;
  effectiveEffort: string;
  effortChoices: readonly string[];
  onEffortChange: (effort: string) => void;
  effectiveMode: string;
  modeChoices: readonly AgentModeOption[];
  onModeChange: (mode: string) => void;
  refIds: string[];
  onRefIdsChange: (ids: string[]) => void;
  projects?: readonly ProjectView[];
  machines?: readonly MachineView[];
  machineId?: string;
  refProminent?: boolean;
  chipsSlot?: ReactNode;
  topSlot?: ReactNode;
}

export type ComposerProps = SessionComposerProps | StandaloneComposerProps;

/** Drafts survive session switches (per-task, in-memory). */
const drafts = new Map<string, string>();

const RING_RADIUS = 5.2;
const RING_CIRC = 2 * Math.PI * RING_RADIUS;

function parseMention(text: string, members: BoardMemberCursor[]): { target: string; body: string } | null {
  const m = /^@([\w.-]+)\s+([\s\S]+)$/.exec(text.trim());
  if (!m) return null;
  if (!members.some((x) => x.memberName === m[1])) return null;
  return { target: m[1], body: m[2].trim() };
}

function PermissionGlyph({ kind, size = 13.5 }: { kind: PermissionIconKind; size?: number }) {
  switch (kind) {
    case 'yolo':
      return <PermYoloIcon size={size} />;
    case 'ask':
      return <PermAskIcon size={size} />;
    case 'plan':
      return <PermPlanIcon size={size} />;
    default:
      return <PermAutoIcon size={size} />;
  }
}

/**
 * Icon-only permission mode picker anchored flush after `Effort`.
 * Gray geometric icons distinguish Ask / Auto / Plan; amber warning icon highlights YOLO / Full.
 */
function PermissionModeControl({
  effectiveMode,
  activeProbe,
  modeChoices,
  disabled,
  onChange,
}: {
  effectiveMode: string;
  activeProbe?: AgentProbe;
  modeChoices: readonly AgentModeOption[];
  disabled?: boolean;
  onChange: (mode: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const btnRef = useRef<HTMLButtonElement | null>(null);
  const popRef = useRef<HTMLDivElement | null>(null);
  const close = useCallback(() => setOpen(false), []);

  const popStyle = useAnchoredPopover({
    open,
    anchorRef: btnRef,
    popRef,
    width: 268,
    align: 'left',
    placement: 'up',
    gap: 8,
    minSpaceUp: 200,
    onClose: close,
  });

  const resolvedModeId = effectiveMode || activeProbe?.defaultMode || '';
  const iconKind = classifyPermissionMode(resolvedModeId);
  const activeOpt = effectiveMode ? modeChoices.find((o) => o.id === effectiveMode) : undefined;
  const defaultLabel = defaultModeDisplayLabel(activeProbe, modeChoices);
  const currentLabel = activeOpt ? modeLabel(activeOpt) : effectiveMode || defaultLabel;

  return (
    <div className="perm-anchor">
      <button
        ref={btnRef}
        type="button"
        disabled={disabled}
        className={`perm-icon-btn ${iconKind === 'yolo' ? 'is-yolo' : ''} ${open ? 'open' : ''}`}
        title={`权限模式：${currentLabel}`}
        aria-label={`权限模式：${currentLabel}`}
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => setOpen((v) => !v)}
      >
        <PermissionGlyph kind={iconKind} size={13.5} />
      </button>
      {open &&
        popStyle &&
        createPortal(
          <div ref={popRef} className="perm-pop" style={popStyle} role="menu" aria-label="Agent 权限模式">
            <div className="ov-menu-sec">Agent 权限模式</div>
            <button
              type="button"
              role="menuitem"
              className={`perm-pop-item ${!effectiveMode ? 'active' : ''}`}
              onClick={() => {
                onChange('');
                close();
              }}
            >
              <div className="perm-item-row">
                <span className={`perm-item-ico ${classifyPermissionMode(activeProbe?.defaultMode) === 'yolo' ? 'is-yolo' : ''}`}>
                  <PermissionGlyph kind={classifyPermissionMode(activeProbe?.defaultMode)} size={12.5} />
                </span>
                <span className="grow ell">{defaultLabel}</span>
                {!effectiveMode && <Check size={12} strokeWidth={2.2} className="ok-t" />}
              </div>
            </button>
            {modeChoices.length === 0 ? (
              <div className="perm-pop-empty">当前 Agent 暂未上报可选权限模式</div>
            ) : (
              modeChoices.map((opt) => {
                const k = classifyPermissionMode(opt.id);
                const selected = effectiveMode === opt.id;
                return (
                  <button
                    key={opt.id}
                    type="button"
                    role="menuitem"
                    className={`perm-pop-item ${selected ? 'active' : ''}`}
                    onClick={() => {
                      onChange(opt.id);
                      close();
                    }}
                  >
                    <div className="perm-item-row">
                      <span className={`perm-item-ico ${k === 'yolo' ? 'is-yolo' : ''}`}>
                        <PermissionGlyph kind={k} size={12.5} />
                      </span>
                      <span className="grow ell">{modeLabel(opt)}</span>
                      {selected && <Check size={12} strokeWidth={2.2} className="ok-t" />}
                    </div>
                    {opt.description && <div className="perm-pop-desc">{opt.description}</div>}
                  </button>
                );
              })
            )}
          </div>,
          document.body,
        )}
    </div>
  );
}

/**
 * Lody-style single 14px ring + percentage micro-button (immediately left of send/stop).
 * Shows real live context usage in active sessions and real 5h/7d agent quotas in New Thread & detail popover.
 */
function LodyTelemetryControl({
  indicatorMode,
  contextUsage,
  agentLabel,
  adapter,
  model,
  machines,
  machineId,
}: {
  indicatorMode: ComposerIndicatorMode;
  contextUsage?: ContextUsage | null;
  agentLabel: string;
  adapter: SessionAdapter | '';
  model: string;
  machines?: readonly MachineView[];
  machineId?: string;
}) {
  const qc = useQueryClient();
  const [open, setOpen] = useState(false);
  const [refreshingQuota, setRefreshingQuota] = useState(false);
  const btnRef = useRef<HTMLButtonElement | null>(null);
  const popRef = useRef<HTMLDivElement | null>(null);
  const close = useCallback(() => setOpen(false), []);

  const popStyle = useAnchoredPopover({
    open,
    anchorRef: btnRef,
    popRef,
    width: 276,
    align: 'right',
    placement: 'up',
    gap: 8,
    minSpaceUp: 230,
    onClose: close,
  });

  const quota = useMemo(
    () => resolveAgentQuota({ machines, machineId, adapter, model }),
    [machines, machineId, adapter, model],
  );

  const handleRefreshQuota = async () => {
    if (!quota.machineId || !quota.machineOnline || !quota.canRefresh || refreshingQuota) return;
    setRefreshingQuota(true);
    try {
      await api.refreshMachineQuotas(quota.machineId);
      void qc.invalidateQueries({ queryKey: ['bootstrap'] });
      void qc.invalidateQueries({ queryKey: ['machines'] });
    } catch (e) {
      toast(errMsg(e), 'err');
    } finally {
      setRefreshingQuota(false);
    }
  };

  const hasContext = !!contextUsage && contextUsage.size > 0;
  const ctxPct = hasContext
    ? Math.min(100, Math.max(0, Math.round((contextUsage.used / contextUsage.size) * 100)))
    : null;

  const isNewQuota = indicatorMode === 'quota';
  const ctxIsWarn = !isNewQuota && ctxPct !== null && ctxPct >= 80;
  const quotaPct = quota.available ? quota.primaryRemainingPercent : null;
  const quotaIsWarn = quotaPct !== null && quotaPct <= 20;

  const primaryPct = isNewQuota ? quotaPct : ctxPct;
  const isWarn = isNewQuota ? quotaIsWarn : ctxIsWarn;
  const dashOffset = primaryPct !== null ? RING_CIRC * (1 - primaryPct / 100) : RING_CIRC;
  const primaryText = primaryPct !== null ? `${primaryPct}%` : '—';

  const ctxSummaryText = isNewQuota
    ? '0 / — · 新会话'
    : hasContext && ctxPct !== null
      ? `${formatTokenCount(contextUsage.used)} / ${formatTokenCount(contextUsage.size)} (${ctxPct}%)`
      : '暂无上下文数据';

  const btnTitle = isNewQuota
    ? quotaPct !== null
      ? `${agentLabel || 'Agent'} 剩余额度 ${quotaPct}% · 点击查看上下文与额度详情`
      : !quota.machineOnline && quota.machineId
        ? `${agentLabel || 'Agent'} 目标设备离线 · 点击查看详情`
        : `${agentLabel || 'Agent'} 额度暂未上报 · 点击查看上下文与额度详情`
    : hasContext && ctxPct !== null
      ? `当前对话上下文 ${formatTokenCount(contextUsage.used)} / ${formatTokenCount(contextUsage.size)} (${ctxPct}%) · 点击查看详情`
      : '当前对话上下文暂未上报 · 点击查看上下文与额度详情';

  const renderQuotaRow = (title: string, win?: QuotaWindow) => {
    if (!win) {
      return (
        <div className="tel-pop-row">
          <div className="tel-pop-row-top">
            <span>{title}</span>
            <span className="tel-pop-val faint">
              {!quota.machineOnline && quota.machineId ? '设备离线' : '暂未上报'}
            </span>
          </div>
          <div className="tel-meter-bar">
            <i style={{ width: '0%' }} />
          </div>
        </div>
      );
    }
    const rem = Math.min(100, Math.max(0, Math.round(win.remainingPercent)));
    const low = rem <= 20;
    const resetText = formatQuotaResetTime(win.resetsAt);
    return (
      <div className="tel-pop-row">
        <div className="tel-pop-row-top">
          <span>{title}</span>
          <span className={`tel-pop-val ${low ? 'is-warn' : ''}`}>剩 {rem}%</span>
        </div>
        <div className={`tel-meter-bar ${low ? 'is-warn' : 'is-accent'}`}>
          <i style={{ width: `${rem}%` }} />
        </div>
        {resetText && <div className="tel-pop-reset">{resetText}</div>}
      </div>
    );
  };

  return (
    <div className="tel-anchor">
      <button
        ref={btnRef}
        type="button"
        className={`tel-btn ${isWarn ? 'is-warn' : ''} ${open ? 'open' : ''}`}
        data-tel-mode={indicatorMode}
        aria-haspopup="dialog"
        aria-expanded={open}
        title={btnTitle}
        aria-label={btnTitle}
        onClick={() => setOpen((v) => !v)}
      >
        <svg className="ctx-ring-svg" viewBox="0 0 14 14" aria-hidden="true">
          <circle className="ctx-ring-track" cx="7" cy="7" r={RING_RADIUS} />
          <circle
            className="ctx-ring-arc"
            cx="7"
            cy="7"
            r={RING_RADIUS}
            strokeDasharray={RING_CIRC.toFixed(2)}
            strokeDashoffset={dashOffset.toFixed(2)}
          />
        </svg>
        <span>{primaryText}</span>
      </button>
      {open &&
        popStyle &&
        createPortal(
          <div ref={popRef} className="tel-pop" style={popStyle} role="dialog" aria-label="上下文与额度详情">
            <div className="tel-pop-sec">
              <div className="tel-pop-hd">
                <span>当前对话上下文</span>
                <span className={`tel-pop-val ${ctxIsWarn ? 'is-warn' : ''}`}>{ctxSummaryText}</span>
              </div>
              <div className={`tel-meter-bar ${ctxIsWarn ? 'is-warn' : hasContext && !isNewQuota ? 'is-accent' : ''}`}>
                <i style={{ width: `${!isNewQuota && ctxPct !== null ? ctxPct : 0}%` }} />
              </div>
            </div>
            <div className="tel-pop-div" />
            <div className="tel-pop-sec">
              <div className="tel-pop-hd">
                <span className="tel-pop-title">
                  <span>Agent 额度</span>
                  {quota.group && (
                    <span className="tel-pop-grp ell" title={quota.group}>
                      {quota.group}
                    </span>
                  )}
                </span>
                <span className="tel-pop-hd-r">
                  <span className="tel-pop-sub">{agentLabel || '—'}</span>
                  {quota.machineId && (
                    <button
                      type="button"
                      className="tel-refresh-btn"
                      disabled={!quota.machineOnline || !quota.canRefresh || refreshingQuota}
                      title={
                        !quota.machineOnline
                          ? '目标设备离线，无法刷新额度'
                          : !quota.canRefresh
                            ? '当前 Runner 暂不支持刷新额度'
                            : '刷新 Agent 额度'
                      }
                      aria-label="刷新 Agent 额度"
                      onClick={() => void handleRefreshQuota()}
                    >
                      <RefreshCw size={11} strokeWidth={1.8} className={refreshingQuota ? 'spin-ico' : ''} />
                    </button>
                  )}
                </span>
              </div>
              {renderQuotaRow('5 小时额度', quota.win5h)}
              {renderQuotaRow('7 天额度', quota.win7d)}
            </div>
          </div>,
          document.body,
        )}
    </div>
  );
}

interface ComposerViewProps {
  standalone?: boolean;
  inDrawer?: boolean;
  compact?: boolean;
  editing?: boolean;
  topSlot?: ReactNode;
  banners?: ReactNode;
  attachRow?: ReactNode;
  chipsRow?: ReactNode;
  value: string;
  placeholder: string;
  autoFocus?: boolean;
  textareaRef: RefObject<HTMLTextAreaElement | null>;
  onChange: (value: string) => void;
  onKeyDown: (e: React.KeyboardEvent<HTMLTextAreaElement>) => void;
  onCompositionStart?: () => void;
  onCompositionEnd?: () => void;
  workerLabel?: string;
  hideRef?: boolean;
  refIds: string[];
  onRefIdsChange: (ids: string[]) => void;
  projects?: readonly ProjectView[];
  machines?: readonly MachineView[];
  machineId?: string;
  currentTaskId?: string;
  refProminent?: boolean;
  controlsDisabled?: boolean;
  availableAdapters: readonly SessionAdapter[];
  effectiveAdapter: SessionAdapter | '';
  onAdapterChange: (adapter: SessionAdapter) => void;
  activeProbe?: AgentProbe;
  effectiveModel: string;
  modelChoices: readonly string[];
  onModelChange: (model: string) => void;
  effectiveEffort: string;
  effortChoices: readonly string[];
  onEffortChange: (effort: string) => void;
  effectiveMode: string;
  modeChoices: readonly AgentModeOption[];
  onModeChange: (mode: string) => void;
  indicatorMode: ComposerIndicatorMode;
  contextUsage?: ContextUsage | null;
  actionSlot: ReactNode;
}

/**
 * Shared visual shell for all Composer instances across the app (New Thread, Main Session,
 * Split Child Drawer, Full Child Page, Hebi8, ReviewOverlay, and Mobile).
 * Guarantees strict left-to-right control order and equal dock height across split views.
 */
function ComposerView({
  standalone = false,
  inDrawer = false,
  compact = false,
  editing = false,
  topSlot,
  banners,
  attachRow,
  chipsRow,
  value,
  placeholder,
  autoFocus = false,
  textareaRef,
  onChange,
  onKeyDown,
  onCompositionStart,
  onCompositionEnd,
  workerLabel,
  hideRef = false,
  refIds,
  onRefIdsChange,
  projects,
  machines,
  machineId,
  currentTaskId,
  refProminent = false,
  controlsDisabled = false,
  availableAdapters,
  effectiveAdapter,
  onAdapterChange,
  activeProbe,
  effectiveModel,
  modelChoices,
  onModelChange,
  effectiveEffort,
  effortChoices,
  onEffortChange,
  effectiveMode,
  modeChoices,
  onModeChange,
  indicatorMode,
  contextUsage,
  actionSlot,
}: ComposerViewProps) {
  const agentDisplay = effectiveAdapter ? ADAPTER_LABELS[effectiveAdapter] ?? effectiveAdapter : '无可用 Agent';
  const modelDisplay = effectiveModel || activeProbe?.defaultModel || '默认模型';
  const effortDisplay = effectiveEffort ? effortLabel(effectiveEffort) : '默认';

  const box = (
    <div className={`composer ${standalone ? 'is-standalone' : ''} ${editing ? 'editing' : ''} ${inDrawer ? 'in-drawer' : ''} ${compact ? 'is-compact' : ''}`}>
      {banners}
      {attachRow}
      {chipsRow}

      <textarea
        ref={textareaRef}
        className={standalone ? 'composer-ta-standalone' : 'composer-ta-dock'}
        value={value}
        rows={1}
        autoFocus={autoFocus}
        placeholder={placeholder}
        onChange={(e) => onChange(e.target.value)}
        onCompositionStart={onCompositionStart}
        onCompositionEnd={onCompositionEnd}
        onKeyDown={onKeyDown}
        aria-label="输入"
      />

      <div className="composer-meta-bar">
        {!workerLabel ? (
          <div className="runtime-controls" aria-label="会话运行配置">
            {!hideRef && (
              <>
                <TaskRefButton
                  selectedIds={refIds}
                  onChange={onRefIdsChange}
                  projects={projects}
                  currentTaskId={currentTaskId}
                  prominent={refProminent}
                  disabled={controlsDisabled}
                />
                <span className="rt-sep">·</span>
              </>
            )}

            <div className="rt-scroll">
              <label className={`rt-pill rt-pill-agent ${controlsDisabled ? 'is-disabled' : ''}`} title={`Agent：${agentDisplay}`}>
                <span className="rt-pill-val">{agentDisplay}</span>
                <ChevronDown size={10} strokeWidth={1.8} className="rt-pill-chev" aria-hidden="true" />
                <select
                  value={effectiveAdapter}
                  disabled={controlsDisabled || availableAdapters.length === 0}
                  onChange={(e) => onAdapterChange(e.target.value as SessionAdapter)}
                  aria-label="Agent"
                >
                  {availableAdapters.length === 0 ? (
                    <option value="">无可用 Agent</option>
                  ) : (
                    availableAdapters.map((a) => (
                      <option key={a} value={a}>
                        {ADAPTER_LABELS[a] ?? a}
                      </option>
                    ))
                  )}
                </select>
              </label>

              <span className="rt-sep">·</span>

              <label className={`rt-pill rt-pill-model ${controlsDisabled ? 'is-disabled' : ''}`} title={`模型：${modelDisplay}`}>
                <span className="rt-pill-val">{modelDisplay}</span>
                <ChevronDown size={10} strokeWidth={1.8} className="rt-pill-chev" aria-hidden="true" />
                <select
                  value={effectiveModel}
                  disabled={controlsDisabled}
                  onChange={(e) => onModelChange(e.target.value)}
                  aria-label="模型"
                >
                  <option value="">
                    {activeProbe?.defaultModel ? `默认 (${activeProbe.defaultModel})` : '跟随 Agent 默认值'}
                  </option>
                  {modelChoices.map((mc) => (
                    <option key={mc} value={mc}>
                      {mc}
                    </option>
                  ))}
                </select>
              </label>

              {effortChoices.length > 0 && (
                <>
                  <span className="rt-sep">·</span>
                  <label className={`rt-pill rt-pill-effort ${controlsDisabled ? 'is-disabled' : ''}`} title={`思考强度：${effortDisplay}`}>
                    <span className="rt-pill-val">{effortDisplay}</span>
                    <ChevronDown size={10} strokeWidth={1.8} className="rt-pill-chev" aria-hidden="true" />
                    <select
                      value={effectiveEffort}
                      disabled={controlsDisabled}
                      onChange={(e) => onEffortChange(e.target.value)}
                      aria-label="思考强度"
                    >
                      <option value="">跟随 Agent 默认值</option>
                      {effortChoices.map((eff) => (
                        <option key={eff} value={eff}>
                          {effortLabel(eff)}
                        </option>
                      ))}
                    </select>
                  </label>
                </>
              )}
            </div>

            <span className="rt-sep">·</span>

            <PermissionModeControl
              effectiveMode={effectiveMode}
              activeProbe={activeProbe}
              modeChoices={modeChoices}
              disabled={controlsDisabled}
              onChange={onModeChange}
            />
          </div>
        ) : (
          <span className="rt-pill">@{workerLabel}</span>
        )}

        <div className="cfoot-acts">
          {!workerLabel && !editing && (
            <LodyTelemetryControl
              indicatorMode={indicatorMode}
              contextUsage={contextUsage}
              agentLabel={effectiveAdapter ? ADAPTER_LABELS[effectiveAdapter] ?? effectiveAdapter : ''}
              adapter={effectiveAdapter}
              model={effectiveModel || activeProbe?.defaultModel || ''}
              machines={machines}
              machineId={machineId}
            />
          )}
          <span className="cfoot-send-slot">{actionSlot}</span>
        </div>
      </div>
    </div>
  );

  if (standalone) {
    return (
      <div className="composer-standalone-wrap">
        {topSlot}
        {box}
      </div>
    );
  }

  return (
    <div className={`cwrap ${inDrawer ? 'in-drawer' : ''} ${compact ? 'is-compact' : ''}`}>
      {topSlot}
      {box}
    </div>
  );
}

function StandaloneComposer(props: StandaloneComposerProps) {
  const internalRef = useRef<HTMLTextAreaElement | null>(null);
  const taRef = props.textareaRef ?? internalRef;
  const ime = useImeGuard();

  const actionSlot = (
    <button
      type="button"
      className={`send-icon-btn ${props.canSend ? 'is-ready' : 'is-idle'}`}
      disabled={!props.canSend}
      title="发送 (Enter)"
      aria-label="发送"
      onClick={props.onSubmit}
    >
      {props.submitting ? <span className="spin" /> : <SendUpIcon size={14} />}
    </button>
  );

  return (
    <ComposerView
      standalone
      topSlot={props.topSlot}
      chipsRow={
        <>
          {props.refIds.length > 0 && (
            <TaskRefChips
              selectedIds={props.refIds}
              onRemove={(id) => props.onRefIdsChange(props.refIds.filter((x) => x !== id))}
              projects={props.projects}
            />
          )}
          {props.chipsSlot}
        </>
      }
      value={props.value}
      placeholder={props.placeholder ?? ''}
      autoFocus={props.autoFocus}
      textareaRef={taRef}
      onChange={props.onChange}
      onCompositionStart={ime.onCompositionStart}
      onCompositionEnd={ime.onCompositionEnd}
      onKeyDown={(e) => {
        if (e.key === 'Enter' && !e.shiftKey && !ime.isImeEnter(e)) {
          e.preventDefault();
          if (props.canSend) props.onSubmit();
        }
      }}
      refIds={props.refIds}
      onRefIdsChange={props.onRefIdsChange}
      projects={props.projects}
      machines={props.machines}
      machineId={props.machineId}
      refProminent={props.refProminent}
      controlsDisabled={props.controlsDisabled}
      availableAdapters={props.availableAdapters}
      effectiveAdapter={props.effectiveAdapter}
      onAdapterChange={props.onAdapterChange}
      activeProbe={props.activeProbe}
      effectiveModel={props.effectiveModel}
      modelChoices={props.modelChoices}
      onModelChange={props.onModelChange}
      effectiveEffort={props.effectiveEffort}
      effortChoices={props.effortChoices}
      onEffortChange={props.onEffortChange}
      effectiveMode={props.effectiveMode}
      modeChoices={props.modeChoices}
      onModeChange={props.onModeChange}
      indicatorMode={props.indicatorMode ?? 'quota'}
      contextUsage={props.contextUsage}
      actionSlot={actionSlot}
    />
  );
}

function SessionComposer({
  task,
  worker,
  members,
  editing,
  needsVerifyAttemptId,
  onEditDone,
  openAnnotations = [],
  runtimeConfig,
  contextUsage,
  indicatorMode = 'context',
  machines = [],
  projects = [],
  machineId,
  compact = false,
  inDrawer = false,
  placeholder: placeholderProp,
  topSlot,
  draftInsert,
  onOpenReview,
}: SessionComposerProps) {
  const qc = useQueryClient();
  const ime = useImeGuard();
  const remember = useWorkbenchStore((s) => s.rememberPrompt);
  const clearDeselected = useWorkbenchStore((s) => s.clearDeselectedAnnotations);
  const [text, setText] = useState(() => drafts.get(task.taskId) ?? '');
  const [siteVerified, setSiteVerified] = useState(false);
  const [adapterOverride, setAdapterOverride] = useState<string>('');
  const [modelOverride, setModelOverride] = useState<string>('');
  const [effortOverride, setEffortOverride] = useState<string>('');
  const [modeOverride, setModeOverride] = useState<string>('');
  const [refIds, setRefIds] = useState<string[]>([]);
  const stash = useRef<string | null>(null);
  const taRef = useRef<HTMLTextAreaElement | null>(null);
  const activeTaskIdRef = useRef(task.taskId);
  activeTaskIdRef.current = task.taskId;
  const busy = isTaskBusy(task);
  const isPA = task.projectId === PA_PROJECT_ID;

  const taskAdapter = (task.preferredAdapter || 'agy') as SessionAdapter;
  const availableAdapters = useMemo(() => {
    const list: SessionAdapter[] = [];
    const seen = new Set<string>();
    for (const mach of machines) {
      if (mach.transportMode === 'ssh_outbound') continue;
      for (const ag of mach.agents ?? []) {
        if (
          ag.state === 'ready' &&
          ag.enabled &&
          (SessionAdapterValues as readonly string[]).includes(ag.adapter) &&
          !seen.has(ag.adapter)
        ) {
          seen.add(ag.adapter);
          list.push(ag.adapter as SessionAdapter);
        }
      }
      for (const adp of mach.adapters ?? []) {
        if ((SessionAdapterValues as readonly string[]).includes(adp) && !seen.has(adp)) {
          seen.add(adp);
          list.push(adp as SessionAdapter);
        }
      }
    }
    if ((SessionAdapterValues as readonly string[]).includes(taskAdapter) && !seen.has(taskAdapter)) {
      list.unshift(taskAdapter);
    }
    if (list.length === 0) list.push('agy');
    return list;
  }, [machines, taskAdapter]);

  const effectiveAdapter = (adapterOverride || taskAdapter) as SessionAdapter;
  const activeProbe = useMemo(
    () => findAgentProbe(machines, machineId, effectiveAdapter),
    [machines, machineId, effectiveAdapter],
  );

  const effectiveModel = modelOverride || runtimeConfig?.model || activeProbe?.defaultModel || '';
  const effectiveEffort =
    effortOverride ||
    runtimeConfig?.thought_level ||
    runtimeConfig?.effort ||
    runtimeConfig?.reasoning_effort ||
    activeProbe?.defaultEffort ||
    '';
  const effectiveMode =
    modeOverride ||
    runtimeConfig?.mode ||
    defaultModeFor(activeProbe, effectiveAdapter, isPA);

  const modelChoices = useMemo(
    () => probeModels(activeProbe, effectiveAdapter, effectiveModel),
    [activeProbe, effectiveAdapter, effectiveModel],
  );
  const effortChoices = useMemo(
    () => probeEfforts(activeProbe, effectiveEffort),
    [activeProbe, effectiveEffort],
  );
  const modeChoices = useMemo(
    () => probeModes(activeProbe, effectiveAdapter, effectiveMode),
    [activeProbe, effectiveAdapter, effectiveMode],
  );

  useEffect(() => {
    setText(drafts.get(task.taskId) ?? '');
    setSiteVerified(false);
    setAdapterOverride('');
    setModelOverride('');
    setEffortOverride('');
    setModeOverride('');
    setRefIds([]);
  }, [task.taskId, needsVerifyAttemptId]);

  useEffect(() => {
    if (!draftInsert) return;
    setText(draftInsert.text);
    drafts.set(task.taskId, draftInsert.text);
    requestAnimationFrame(() => taRef.current?.focus());
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [draftInsert?.id]);

  useEffect(() => {
    if (editing) {
      stash.current = text;
      setText(editing.body);
      requestAnimationFrame(() => taRef.current?.focus());
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [editing?.inputId]);

  const grow = useCallback(() => {
    const el = taRef.current;
    if (!el) return;
    el.style.height = '30px';
    el.style.height = `${Math.min(Math.max(el.scrollHeight, 30), 140)}px`;
  }, []);
  useEffect(grow, [text, grow]);
  useEffect(() => {
    const el = taRef.current;
    if (!el) return;
    let lastW = Math.round(el.getBoundingClientRect().width);
    const ro = new ResizeObserver(() => {
      const nextW = Math.round(el.getBoundingClientRect().width);
      if (nextW > 0 && nextW !== lastW) {
        lastW = nextW;
        grow();
      }
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, [grow]);

  const setDraft = (v: string) => {
    setText(v);
    if (!editing) drafts.set(task.taskId, v);
  };

  const refresh = (
    targetTaskId = task.taskId,
    targetParentId = task.parentTaskId,
    targetProjectId = task.projectId,
  ) => {
    void qc.invalidateQueries({ queryKey: ['task-timeline', targetTaskId] });
    if (targetParentId) {
      void qc.invalidateQueries({ queryKey: ['task-timeline', targetParentId] });
      void qc.invalidateQueries({ queryKey: ['task-children', targetParentId] });
    }
    void qc.invalidateQueries({ queryKey: ['task-children', targetTaskId] });
    void qc.invalidateQueries({ queryKey: ['project-tasks', targetProjectId] });
    void qc.invalidateQueries({ queryKey: ['all-tasks'] });
    void qc.invalidateQueries({ queryKey: ['delivery'] });
    void qc.invalidateQueries({ queryKey: ['global-artifacts'] });
    if (targetProjectId === PA_PROJECT_ID) {
      void qc.invalidateQueries({ queryKey: ['hebi8-main'] });
      void qc.invalidateQueries({ queryKey: ['hebi8-line'] });
    }
    void qc.invalidateQueries({ queryKey: ['bootstrap'] });
  };

  const finishEdit = () => {
    const restored = stash.current ?? '';
    stash.current = null;
    setText(restored);
    drafts.set(task.taskId, restored);
    onEditDone();
  };

  const buildRuntimePayload = () => {
    const cfg: Record<string, string> = {};
    if (modelOverride) cfg.model = modelOverride;
    if (effortOverride) cfg.thought_level = effortOverride;
    if (modeOverride) cfg.mode = modeOverride;
    const explicitFullPermission = modeOverride !== '' && isFullBypassMode(modeOverride) ? true : undefined;
    return {
      adapter: adapterOverride ? (adapterOverride as SessionAdapter) : undefined,
      config: Object.keys(cfg).length > 0 ? cfg : undefined,
      explicitFullPermission,
    };
  };

  const saveEdit = useMutation({
    mutationFn: (v: {
      submittedTaskId: string;
      submittedParentId?: string;
      submittedProjectId: string;
      inputId: string;
      body: string;
    }) => api.updateQueueInput(v.submittedTaskId, v.inputId, v.body),
    onSuccess: (pi, v) => {
      remember(`inp:${pi.inputId}`, pi.body);
      toast('已更新');
      refresh(v.submittedTaskId, v.submittedParentId, v.submittedProjectId);
      if (activeTaskIdRef.current === v.submittedTaskId) {
        finishEdit();
      }
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const followup = useMutation({
    mutationFn: (v: {
      submittedTaskId: string;
      submittedParentId?: string;
      submittedProjectId: string;
      message: string;
      mode: FollowupMode;
      referencedTaskIds?: string[];
      verifiedSite?: boolean;
      verifiedAttemptId?: string;
    }) => {
      const rt = buildRuntimePayload();
      return api.followup(v.submittedTaskId, {
        message: v.message,
        mode: v.mode,
        adapter: rt.adapter,
        config: rt.config,
        explicitFullPermission: rt.explicitFullPermission,
        referencedTaskIds: v.referencedTaskIds && v.referencedTaskIds.length > 0 ? v.referencedTaskIds : undefined,
        verifiedSite: v.verifiedSite,
        verifiedAttemptId: v.verifiedAttemptId,
      });
    },
    onSuccess: (res, v) => {
      if (res.attempt) remember(`att:${res.attempt.attemptId}`, v.message);
      if (res.pendingInput) remember(`inp:${res.pendingInput.inputId}`, v.message);
      if (res.pendingInput && v.mode === 'queue') toast('将在下一轮发出');
      if (v.mode === 'steer') toast('已打断当前轮，优先执行');
      drafts.set(v.submittedTaskId, '');
      if (activeTaskIdRef.current === v.submittedTaskId) {
        setText('');
        setRefIds([]);
        setSiteVerified(false);
        setModeOverride((cur) => nextModeOverrideAfterSend(cur));
      }
      refresh(v.submittedTaskId, v.submittedParentId, v.submittedProjectId);
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const dispatchAnns = useMutation({
    mutationFn: (v: {
      submittedTaskId: string;
      submittedParentId?: string;
      submittedProjectId: string;
      ids: string[];
      message: string;
      mode: FollowupMode;
    }) =>
      api.dispatchAnnotations(v.submittedTaskId, {
        annotationIds: v.ids,
        message: v.message || undefined,
        mode: v.mode,
      }),
    onSuccess: (res, v) => {
      clearDeselected(v.submittedTaskId);
      drafts.set(v.submittedTaskId, '');
      if (activeTaskIdRef.current === v.submittedTaskId) {
        setText('');
        setSiteVerified(false);
      }
      if (res.attempt) toast(`已连同 ${v.ids.length} 条批注发出`);
      else if (res.pendingInput) toast(`已入队 · ${v.ids.length} 条批注`);
      refresh(v.submittedTaskId, v.submittedParentId, v.submittedProjectId);
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const boardPost = useMutation({
    mutationFn: (v: { submittedTaskId: string; target: string; body: string }) =>
      api.postBoardMessage(v.submittedTaskId, { authorName: 'human', targetMention: v.target, body: v.body }),
    onSuccess: (msg, v) => {
      toast(`已发给 @${msg.targetMention ?? ''}`);
      drafts.set(v.submittedTaskId, '');
      if (activeTaskIdRef.current === v.submittedTaskId) {
        setText('');
      }
      void qc.invalidateQueries({ queryKey: ['task-board', v.submittedTaskId] });
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const cancel = useMutation({
    mutationFn: () => api.cancelTask(task.taskId, 'stopped from composer'),
    onSuccess: (r) => {
      toast(r.cancelSent || r.immediateState ? '已请求停止' : '停止请求已记录');
      refresh();
    },
    onError: (e) => toast(errMsg(e), 'err'),
  });

  const pending = followup.isPending || dispatchAnns.isPending || boardPost.isPending || saveEdit.isPending;
  const needsVerifyGate = !editing && !worker && !busy && !!needsVerifyAttemptId;
  const hasAttachments = !editing && !worker && openAnnotations.length > 0;
  const hasPayload = !!text.trim() || hasAttachments;

  const submit = (steer: boolean) => {
    const msg = text.trim();
    if (pending) return;
    const submittedTaskId = task.taskId;
    const submittedParentId = task.parentTaskId;
    const submittedProjectId = task.projectId;
    if (editing) {
      if (!msg) return;
      saveEdit.mutate({
        submittedTaskId,
        submittedParentId,
        submittedProjectId,
        inputId: editing.inputId,
        body: msg,
      });
      return;
    }
    if (worker) {
      if (!msg) return;
      boardPost.mutate({ submittedTaskId, target: worker, body: msg });
      return;
    }
    if (msg) {
      const mention = parseMention(msg, members);
      if (mention) {
        boardPost.mutate({ submittedTaskId, ...mention });
        return;
      }
    }
    if (!hasPayload) return;
    if (needsVerifyGate && !siteVerified) {
      toast('先看一眼工作区并勾选「看过了」再继续', 'err');
      return;
    }
    const mode: FollowupMode = steer && busy ? 'steer' : 'queue';
    if (hasAttachments) {
      dispatchAnns.mutate({
        submittedTaskId,
        submittedParentId,
        submittedProjectId,
        ids: openAnnotations.map((a) => a.annotationId),
        message: msg,
        mode,
      });
      return;
    }
    followup.mutate({
      submittedTaskId,
      submittedParentId,
      submittedProjectId,
      message: msg,
      mode,
      referencedTaskIds: refIds.length > 0 ? refIds : undefined,
      verifiedSite: needsVerifyGate ? true : undefined,
      verifiedAttemptId: needsVerifyGate ? needsVerifyAttemptId : undefined,
    });
  };

  let placeholder = placeholderProp ?? '发消息继续对话…';
  if (editing) placeholder = '编辑这条…';
  else if (worker) placeholder = `发给 @${worker}`;
  else if (busy) placeholder = 'Enter 入队 · ⌘↵ 立刻打断';
  else if (isPA && !placeholderProp) placeholder = '问问 Hebi8，或点击左下角引用对话上下文…';

  const renderActionSlot = () => {
    if (editing) {
      return (
        <>
          <button type="button" className="btn ghost sm" onClick={finishEdit}>
            取消
          </button>
          <button
            type="button"
            className={`send-icon-btn ${text.trim() && !pending ? 'is-ready' : 'is-idle'}`}
            disabled={!text.trim() || pending}
            title="保存 (Enter)"
            aria-label="保存"
            onClick={() => submit(false)}
          >
            {saveEdit.isPending ? <span className="spin" /> : <Check size={14} strokeWidth={2} />}
          </button>
        </>
      );
    }

    if (busy && !hasPayload && !worker) {
      return (
        <button
          type="button"
          className="send-icon-btn is-stop"
          disabled={cancel.isPending}
          title="停止当前轮"
          aria-label="停止当前轮"
          onClick={() => cancel.mutate()}
        >
          {cancel.isPending ? <span className="spin" /> : <StopSquareIcon size={12} />}
        </button>
      );
    }

    if (busy && hasPayload && !worker) {
      return (
        <>
          <button
            type="button"
            className="send-icon-btn is-stop"
            disabled={cancel.isPending}
            title="停止当前轮"
            aria-label="停止当前轮"
            onClick={() => cancel.mutate()}
          >
            {cancel.isPending ? <span className="spin" /> : <StopSquareIcon size={12} />}
          </button>
          <button
            type="button"
            className="send-icon-btn is-queue"
            disabled={pending}
            title="入队等待当前轮结束 (⌘↵ 立即打断)"
            aria-label="入队发送"
            onClick={() => submit(false)}
          >
            {pending ? <span className="spin" /> : <QueueSendIcon size={14} />}
          </button>
        </>
      );
    }

    const canSend = hasPayload && !pending && !(needsVerifyGate && !siteVerified);
    return (
      <button
        type="button"
        className={`send-icon-btn ${canSend ? 'is-ready' : 'is-idle'}`}
        disabled={!canSend}
        title={hasAttachments ? '连同 Review 批注一起发送 (Enter)' : '发送 (Enter)'}
        aria-label="发送"
        onClick={() => submit(false)}
      >
        {pending ? <span className="spin" /> : <SendUpIcon size={14} />}
      </button>
    );
  };

  return (
    <ComposerView
      inDrawer={inDrawer}
      compact={compact}
      editing={!!editing}
      topSlot={topSlot}
      banners={
        <>
          {editing && (
            <div className="cbanner">
              <Pencil size={12} strokeWidth={1.6} /> 编辑中
              <span className="r">Esc 取消</span>
            </div>
          )}
          {worker && !editing && (
            <div className="cbanner" style={{ color: 'var(--muted)' }}>
              <SwarmIcon size={12} /> 发给 <b style={{ color: 'var(--fg)' }}>@{worker}</b>
            </div>
          )}
          {needsVerifyGate && (
            <div className="cbanner">
              <span>上一轮中途断了，先看一眼工作区</span>
              <label className="chk" style={{ marginLeft: 'auto', cursor: 'pointer' }}>
                <input type="checkbox" checked={siteVerified} onChange={(e) => setSiteVerified(e.target.checked)} />
                看过了
              </label>
            </div>
          )}
        </>
      }
      attachRow={
        hasAttachments ? (
          <div className="composer-attach-row">
            <button
              type="button"
              className="attach-chip"
              title="发送将一并附带这些 Review 批注"
              onClick={() => onOpenReview?.()}
            >
              <MessageSquare size={12} strokeWidth={1.8} />
              <span>{openAnnotations.length} 条 Review 批注</span>
            </button>
          </div>
        ) : undefined
      }
      chipsRow={
        !editing && !worker && refIds.length > 0 ? (
          <TaskRefChips
            selectedIds={refIds}
            onRemove={(id) => setRefIds((prev) => prev.filter((x) => x !== id))}
            projects={projects}
          />
        ) : undefined
      }
      value={text}
      placeholder={placeholder}
      textareaRef={taRef}
      onChange={setDraft}
      onCompositionStart={ime.onCompositionStart}
      onCompositionEnd={ime.onCompositionEnd}
      onKeyDown={(e) => {
        if (ime.isImeEnter(e)) return;
        if (e.key === 'Escape' && editing) {
          e.preventDefault();
          e.stopPropagation();
          finishEdit();
          return;
        }
        if (e.key === 'Enter' && (e.metaKey || e.ctrlKey)) {
          e.preventDefault();
          submit(true);
          return;
        }
        if (e.key === 'Enter' && !e.shiftKey) {
          e.preventDefault();
          submit(false);
        }
      }}
      workerLabel={worker}
      hideRef={!!editing}
      refIds={refIds}
      onRefIdsChange={setRefIds}
      projects={projects}
      machines={machines}
      machineId={machineId}
      currentTaskId={task.taskId}
      refProminent={isPA}
      availableAdapters={availableAdapters}
      effectiveAdapter={effectiveAdapter}
      onAdapterChange={(next) => {
        setAdapterOverride(next);
        setModelOverride('');
        setEffortOverride('');
        setModeOverride('');
      }}
      activeProbe={activeProbe}
      effectiveModel={effectiveModel}
      modelChoices={modelChoices}
      onModelChange={setModelOverride}
      effectiveEffort={effectiveEffort}
      effortChoices={effortChoices}
      onEffortChange={setEffortOverride}
      effectiveMode={effectiveMode}
      modeChoices={modeChoices}
      onModeChange={setModeOverride}
      indicatorMode={indicatorMode}
      contextUsage={contextUsage}
      actionSlot={renderActionSlot()}
    />
  );
}

export function Composer(props: ComposerProps) {
  if (props.standalone) {
    return <StandaloneComposer {...props} />;
  }
  return <SessionComposer {...props} />;
}
