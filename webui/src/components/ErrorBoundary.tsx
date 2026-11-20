import { Component, type ErrorInfo, type ReactNode } from "react";

import { Button } from "./ui/primitives";
import { useI18n } from "@/i18n";

type Props = { fallback: (reset: () => void) => ReactNode; children: ReactNode };
type State = { failed: boolean };

// Boundary 必须是 class 组件；文案由外层函数组件通过 fallback 注入，避免在这里碰 hooks。
class Boundary extends Component<Props, State> {
  state: State = { failed: false };

  static getDerivedStateFromError(): State {
    return { failed: true };
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error("route render failed", error, info.componentStack);
  }

  reset = () => this.setState({ failed: false });

  render() {
    return this.state.failed ? this.props.fallback(this.reset) : this.props.children;
  }
}

/**
 * ErrorBoundary 兜住页面渲染失败——最常见的是重新部署后旧标签页请求到已删除的
 * chunk，动态 import 被拒绝，此时没有边界整棵树会卸载成白屏。
 */
export function ErrorBoundary({ children }: { children: ReactNode }) {
  const { t } = useI18n();
  return (
    <Boundary
      fallback={() => (
        <div className="rounded-xl border border-red-200 bg-red-50 p-6" role="alert">
          <p className="text-sm font-medium text-red-700">{t("error.title")}</p>
          <p className="mt-1 text-sm text-red-600">{t("error.hint")}</p>
          <Button className="mt-3" onClick={() => window.location.reload()}>
            {t("error.reload")}
          </Button>
        </div>
      )}
    >
      {children}
    </Boundary>
  );
}
