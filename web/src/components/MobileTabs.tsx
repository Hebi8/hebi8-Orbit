import { Layers, LayoutGrid, Plus } from 'lucide-react';
import { navigate, paths, type Route } from '../lib/router';
import { PAIcon } from './Icons';

/** < 768px: fixed 4-tab bar (总览 / Hebi8 / Artifacts / 新建), aligned with v4 mobile spec. */
export function MobileTabs({ route, hasUrgent }: { route: Route; hasUrgent: boolean }) {
  const tab =
    route.name === 'home'
      ? 'home'
      : route.name === 'pa'
        ? 'pa'
        : route.name === 'artifacts'
          ? 'artifacts'
          : route.name === 'new'
            ? 'new'
            : '';
  return (
    <nav className="mtabs" aria-label="主导航">
      <button
        type="button"
        className={tab === 'home' ? 'on' : ''}
        aria-current={tab === 'home' ? 'page' : undefined}
        onClick={() => navigate(paths.home())}
      >
        <LayoutGrid size={17} strokeWidth={1.6} />
        总览
        {hasUrgent && <span className="mdot" aria-label="有待处理会话" />}
      </button>
      <button
        type="button"
        className={tab === 'pa' ? 'on' : ''}
        aria-current={tab === 'pa' ? 'page' : undefined}
        onClick={() => navigate(paths.hebi8())}
      >
        <PAIcon size={17} />
        Hebi8
      </button>
      <button
        type="button"
        className={tab === 'artifacts' ? 'on' : ''}
        aria-current={tab === 'artifacts' ? 'page' : undefined}
        onClick={() => navigate(paths.artifacts())}
      >
        <Layers size={17} strokeWidth={1.6} />
        Artifacts
      </button>
      <button
        type="button"
        className={tab === 'new' ? 'on' : ''}
        aria-current={tab === 'new' ? 'page' : undefined}
        onClick={() => navigate(paths.newThread())}
      >
        <Plus size={17} strokeWidth={1.6} />
        新建
      </button>
    </nav>
  );
}
