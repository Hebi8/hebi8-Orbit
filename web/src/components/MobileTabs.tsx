import { FolderGit2, LayoutGrid } from 'lucide-react';
import { navigate, paths, type Route } from '../lib/router';
import { PAIcon } from './Icons';

/** < 768px: fixed 3-tab bar (总览 / 项目 / PA), safe-area aware. */
export function MobileTabs({ route, inboxCount }: { route: Route; inboxCount: number }) {
  const tab = route.name === 'home' ? 'home' : route.name === 'pa' ? 'pa' : route.name === 'new' ? '' : 'projects';
  return (
    <nav className="mtabs" aria-label="主导航">
      <button type="button" className={tab === 'home' ? 'on' : ''} onClick={() => navigate(paths.home())}>
        <LayoutGrid size={18} strokeWidth={1.6} />
        总览
        {inboxCount > 0 && <span className="badge">{inboxCount}</span>}
      </button>
      <button type="button" className={tab === 'projects' ? 'on' : ''} onClick={() => navigate(paths.projects())}>
        <FolderGit2 size={18} strokeWidth={1.6} />
        项目
      </button>
      <button type="button" className={tab === 'pa' ? 'on' : ''} onClick={() => navigate(paths.pa())}>
        <PAIcon size={18} />
        PA
      </button>
    </nav>
  );
}
