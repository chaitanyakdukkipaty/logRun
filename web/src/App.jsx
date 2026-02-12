import { Routes, Route } from 'react-router-dom'
import Layout from './components/Layout'
import ProcessList from './pages/ProcessList'
import ProcessDetail from './pages/ProcessDetail'

function App() {
  return (
    <Layout>
      <Routes>
        <Route path="/" element={<ProcessList />} />
        <Route path="/processes/:id" element={<ProcessDetail />} />
      </Routes>
    </Layout>
  )
}

export default App