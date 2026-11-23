import { useEffect, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import type { InboxItemView } from './generated/protocol';
import { api } from './lib/api';
import { useRealtimeSync } from './hooks/useRealtimeSync';
import { useWorkbenchStore } from './store/useWorkbenchStore';
import { TopBar } from './components/TopBar';
import { LeftSidebar } from './components/LeftSidebar';
import { TaskTimelinePane } from './components/TaskTimelinePane';
import { ReviewDrawer } from './components/ReviewDrawer';
import { MobileShell } from './components/MobileShell';
import { PAPlaceholderModal } from './components/PAPlaceholderModal';

export function App() {
  const {
    selectedProjectId,
    setSelectedProject,
    selectedTaskId,
    setSelectedTask,
    selectedDeliveryId,
    setSelectedDelivery,
    theme,
    setShowPAOverlay,
  } = useWorkbenchStore();

  const [isDark, setIsDark] = useState(true);

  // Sync theme class with document root (dark by default, respecting system mode)
  useEffect(() => {
    const mq = window.matchMedia('(prefers-color-scheme: dark)');
    const apply = () => {
      const dark = theme === 'dark' || (theme === 'system' && mq.matches);
      setIsDark(dark);
      document.documentElement.classList.toggle('dark', dark);
    };
    apply();
    mq.addEventListener('change', apply);
    return () => mq.removeEventListener('change', apply);
  }, [theme]);

  // Global Cmd+K / Ctrl+K shortcut for PA control drawer
  useEffect(() => {
    const onKeyDown = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault();
        setShowPAOverlay(true);
      }
    };
    window.addEventListener('keydown', onKeyDown);
    return () => window.removeEventListener('keydown', onKeyDown);
  }, [setShowPAOverlay]);

  // 1. Single-transaction bootstrap snapshot
  const bootstrapQuery = useQuery({
    queryKey: ['bootstrap'],
    queryFn: api.bootstrap,
    refetchOnWindowFocus: false,
  });

  // 2. Gapless /ws/client?sinceSeq=<snapshotSeq> realtime subscription
  useRealtimeSync(bootstrapQuery.data?.snapshotSeq);

  const projects = bootstrapQuery.data?.projects ?? [];
  const machines = bootstrapQuery.data?.machines ?? [];
  const inboxItems = bootstrapQuery.data?.openInboxItems ?? [];

  // Auto-select first project once bootstrap loads
  useEffect(() => {
    if (!selectedProjectId && projects.length > 0) {
      setSelectedProject(projects[0].projectId);
    }
  }, [projects, selectedProjectId, setSelectedProject]);

  // 3. Project tasks
  const tasksQuery = useQuery({
    queryKey: ['project-tasks', selectedProjectId],
    queryFn: () => api.listProjectTasks(selectedProjectId!),
    enabled: !!selectedProjectId,
  });
  const tasks = tasksQuery.data ?? [];

  // Auto-select first task when project changes
  useEffect(() => {
    if (selectedProjectId && !selectedTaskId && tasks.length > 0) {
      setSelectedTask(tasks[0].taskId);
    }
  }, [selectedProjectId, selectedTaskId, tasks, setSelectedTask]);

  // 4. Task timeline
  const timelineQuery = useQuery({
    queryKey: ['task-timeline', selectedTaskId],
    queryFn: () => api.getTaskTimeline(selectedTaskId!),
    enabled: !!selectedTaskId,
  });

  // Auto-select latest delivery when timeline loads
  useEffect(() => {
    const deliveries = timelineQuery.data?.deliveries ?? [];
    if (!selectedDeliveryId && deliveries.length > 0) {
      const latest = deliveries[deliveries.length - 1];
      const hasRyugi = (latest.ryugiBindings?.length ?? 0) > 0;
      setSelectedDelivery(latest.deliveryId, hasRyugi ? 'ryugi' : 'diff');
    }
  }, [timelineQuery.data?.deliveries, selectedDeliveryId, setSelectedDelivery]);

  // 5. Selected delivery detail
  const deliveryDetailQuery = useQuery({
    queryKey: ['delivery', selectedDeliveryId],
    queryFn: () => api.getDelivery(selectedDeliveryId!),
    enabled: !!selectedDeliveryId,
  });

  const activeProject = projects.find((p) => p.projectId === selectedProjectId);

  const handleSelectInboxItem = (item: InboxItemView) => {
    setSelectedProject(item.projectId);
    if (item.taskId) {
      setSelectedTask(item.taskId);
    }
    if (item.category === 'delivery_review' && item.refId) {
      setSelectedDelivery(item.refId, 'diff');
    }
  };

  if (bootstrapQuery.isError) {
    return (
      <div className="flex h-full items-center justify-center p-6 text-center">
        <div className="max-w-md rounded-lg border border-rose-500/30 bg-rose-500/10 p-5 text-sm text-rose-500">
          <div className="font-semibold">无法连接 orbit-hub</div>
          <p className="mt-1 text-xs opacity-80">
            {(bootstrapQuery.error as Error).message}
          </p>
        </div>
      </div>
    );
  }

  return (
    <div className="flex h-full flex-col overflow-hidden bg-zinc-50 text-zinc-900 dark:bg-zinc-950 dark:text-zinc-100">
      <TopBar
        activeProject={activeProject}
        machines={machines}
        openInboxCount={inboxItems.length}
      />

      {/* Desktop 3-Pane Industrial Workbench */}
      <div className="hidden min-h-0 flex-1 overflow-hidden md:flex">
        <LeftSidebar
          projects={projects}
          machines={machines}
          inboxItems={inboxItems}
          onSelectInboxItem={handleSelectInboxItem}
        />

        <TaskTimelinePane
          project={activeProject}
          machines={machines}
          tasks={tasks}
          inboxItems={inboxItems}
          timeline={timelineQuery.data}
          isLoadingTimeline={timelineQuery.isLoading}
        />

        <ReviewDrawer
          key={selectedDeliveryId ?? 'none'}
          detail={deliveryDetailQuery.data}
          isLoading={deliveryDetailQuery.isLoading}
          allTaskDeliveries={timelineQuery.data?.deliveries ?? []}
          machines={machines}
          isDark={isDark}
        />
      </div>

      {/* Mobile 3-Tab Responsive Shell */}
      <MobileShell
        projects={projects}
        inboxItems={inboxItems}
        tasks={tasks}
        timeline={timelineQuery.data}
        deliveryDetail={deliveryDetailQuery.data}
        onSelectInboxItem={handleSelectInboxItem}
      />

      <PAPlaceholderModal bootstrap={bootstrapQuery.data} />
    </div>
  );
}

export default App;
