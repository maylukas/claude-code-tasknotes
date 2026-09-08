import { createRootRoute, Outlet } from '@tanstack/react-router'
import { StatusProvider } from '@/lib/StatusContext'
import { BuildHashFooter } from '@/lib/BuildHashFooter'

export const Route = createRootRoute({
  component: RootLayout,
})

function RootLayout() {
  return (
    <StatusProvider>
      <div className="flex min-h-screen flex-col bg-background text-foreground">
        <div className="flex-1">
          <Outlet />
        </div>
        <BuildHashFooter />
      </div>
    </StatusProvider>
  )
}
